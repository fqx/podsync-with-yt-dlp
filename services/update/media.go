package update

import (
	"context"
	"os"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
)

type mediaClaim struct{ originalID, canonical bool }
type mediaClaims map[string]map[string]mediaClaim

var errMediaNameCollision = errors.New("no unambiguous media filename available")

// Original IDs are authoritative over weak trimmed aliases. A configured name
// claimed by multiple episodes is never trusted, linked into, or downloaded over.
func canUseMediaName(episode *model.Episode, name string, claims mediaClaims) bool {
	own := claims[name][episode.ID]
	for id, claim := range claims[name] {
		if id != episode.ID && (claim.canonical || claim.originalID || !own.originalID) {
			return false
		}
	}
	return true
}

func mediaDestination(cfg *feed.Config, episode *model.Episode, claims mediaClaims) (string, error) {
	for _, name := range feed.EpisodeNameCandidates(cfg, episode) {
		if canUseMediaName(episode, name, claims) {
			return name, nil
		}
	}
	return "", errMediaNameCollision
}

func claimMedia(cfg *feed.Config, episodes []*model.Episode) mediaClaims {
	claims := make(mediaClaims)
	for _, episode := range episodes {
		for index, name := range feed.EpisodeNameCandidates(cfg, episode) {
			if claims[name] == nil {
				claims[name] = make(map[string]mediaClaim)
			}
			claims[name][episode.ID] = mediaClaim{originalID: name == feed.LegacyEpisodeName(cfg, episode), canonical: index == 0}
		}
	}
	return claims
}

// Resolve only names with unambiguous ownership. Migration never overwrites an
// existing destination and retains the old file URL through a hard link.
func (u *Manager) resolveMedia(ctx context.Context, cfg *feed.Config, episode *model.Episode, claims mediaClaims) (string, int64, error) {
	destination, err := mediaDestination(cfg, episode, claims)
	if err != nil {
		return "", 0, err
	}
	names := feed.EpisodeNameCandidates(cfg, episode)
	for _, name := range names {
		if !canUseMediaName(episode, name, claims) {
			continue
		}
		size, err := u.fs.Size(ctx, cfg.ID+"/"+name)
		if err == nil {
			resolved := name
			if linker, ok := u.fs.(fs.MediaLinker); ok {
				// Keep both older URL conventions available, even if only one name exists.
				for _, alias := range names {
					if alias == name || !canUseMediaName(episode, alias, claims) {
						continue
					}
					if err := linker.LinkMedia(ctx, cfg.ID+"/"+name, cfg.ID+"/"+alias); err != nil {
						log.WithError(err).Warn("could not add compatible media name; retaining existing media URL")
					} else if alias == destination {
						resolved = destination
					}
				}
			}
			return resolved, size, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", 0, err
		}
	}
	return "", 0, os.ErrNotExist
}

// Artwork reclamation is conservative: a shared historical alias is still media
// on disk, even when it cannot safely identify one episode for RSS/download reuse.
func (u *Manager) hasMedia(ctx context.Context, cfg *feed.Config, episode *model.Episode) (bool, error) {
	for _, name := range feed.EpisodeNameCandidates(cfg, episode) {
		_, err := u.fs.Size(ctx, cfg.ID+"/"+name)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (u *Manager) deleteMedia(ctx context.Context, cfg *feed.Config, episode *model.Episode, protected mediaClaims) error {
	var result *multierror.Error
	for _, name := range feed.EpisodeNameCandidates(cfg, episode) {
		if len(protected[name]) > 0 {
			continue
		}
		if err := u.fs.Delete(ctx, cfg.ID+"/"+name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}
