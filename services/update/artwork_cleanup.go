package update

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"

	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
)

const artworkVersion = "square-blur-dark15-v1"

var artworkVersions = []string{"square-v1", "square-blur-v1", "square-blur-dark30-v1", artworkVersion}
var ownedArtworkName = regexp.MustCompile(`^artwork/(square-v1|square-blur-v1|square-blur-dark30-v1|square-blur-dark15-v1)/[a-f0-9]{64}/[a-f0-9]{64}\.jpg(\.ready)?$`)

func (u *Manager) deleteEpisodeArtwork(ctx context.Context, feedID, episodeID string, legacyOnly bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result *multierror.Error
	protected := u.protectedArtwork()
	for _, version := range artworkVersions {
		if legacyOnly && version == artworkVersion {
			continue
		}
		name := strings.Replace(episodeArtworkPath(feedID, episodeID), "/"+artworkVersion+"/", "/"+version+"/", 1)
		if protected[name] {
			continue
		}
		if err := u.deleteArtworkObjects(ctx, name); err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}

// Remove one version only; failed generation must not remove an old RSS image.
func (u *Manager) deleteArtworkObjects(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if u.protectedArtwork()[name] {
		return nil
	}
	var result *multierror.Error
	for _, object := range []string{name + ".ready", name} {
		if err := u.fs.Delete(ctx, object); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = multierror.Append(result, fmt.Errorf("delete artwork %s: %w", object, err))
		}
	}
	return result.ErrorOrNil()
}

// PruneArtwork reclaims only recognized generated objects. Database and media
// checks complete before enumeration, so an unavailable database cannot authorize
// deletion. Live older versions survive until that feed's RSS is regenerated.
func (u *Manager) PruneArtwork(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	walker, ok := u.fs.(fs.Walker)
	if !ok {
		return nil
	}
	live := make(map[string]bool)
	for feedID, cfg := range u.feeds {
		f, err := u.db.GetFeed(ctx, feedID)
		if errors.Is(err, model.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, episode := range f.Episodes {
			if episode.Status != model.EpisodeDownloaded {
				continue
			}
			exists, err := u.hasMedia(ctx, cfg, episode)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			for _, version := range artworkVersions {
				name := strings.Replace(episodeArtworkPath(feedID, episode.ID), "/"+artworkVersion+"/", "/"+version+"/", 1)
				live[name] = true
			}
		}
	}
	var result *multierror.Error
	protected := u.protectedArtwork()
	for _, version := range artworkVersions {
		err := walker.Walk(ctx, "artwork/"+version, func(name string) error {
			if !ownedArtworkName.MatchString(name) {
				return nil
			}
			base := strings.TrimSuffix(name, ".ready")
			if protected[base] {
				return nil
			}
			if live[base] {
				size, err := u.fs.Size(ctx, base)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				ready, markerErr := u.fs.Size(ctx, base+".ready")
				if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
					return markerErr
				}
				if err == nil && markerErr == nil && size > 0 && ready == 1 {
					return nil
				}
			}
			if err := u.fs.Delete(ctx, name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
		if err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}

// Explicit fixed episode/channel artwork references remain user-owned choices,
// even when they happen to refer to an older generated object on this host.
func (u *Manager) protectedArtwork() map[string]bool {
	protected := make(map[string]bool)
	base, err := url.Parse(u.hostname)
	if err != nil {
		return protected
	}
	prefix := strings.TrimRight(base.Path, "/") + "/"
	for _, cfg := range u.feeds {
		for _, raw := range []string{cfg.Custom.EpisodeCoverArt, cfg.Custom.CoverArt} {
			parsed, err := url.Parse(raw)
			if err != nil || !sameArtworkHost(parsed, base) || !strings.HasPrefix(parsed.Path, prefix) {
				continue
			}
			name := strings.TrimPrefix(parsed.Path, prefix)
			if ownedArtworkName.MatchString(name) {
				protected[strings.TrimSuffix(name, ".ready")] = true
			}
		}
	}
	return protected
}

func sameArtworkHost(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
