package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/artwork"
	"github.com/mxpv/podsync/pkg/builder"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
)

// Hash both identifiers so neither can introduce path components, URL escapes or collisions.
func episodeArtworkPath(feedID, episodeID string) string {
	return fmt.Sprintf("artwork/square-blur-dark15-v1/%x/%x.jpg", sha256.Sum256([]byte(feedID)), sha256.Sum256([]byte(episodeID)))
}

func (u *Manager) lookupThumbnails(ctx context.Context, ids []string) (map[string][]string, error) {
	if u.thumbnailLookup != nil {
		return u.thumbnailLookup(ctx, ids)
	}
	keys := u.keys[model.ProviderYoutube]
	if keys == nil {
		return nil, fmt.Errorf("YouTube key provider unavailable")
	}
	yt, err := builder.NewYouTubeBuilder(keys.Get(), nil)
	if err != nil {
		return nil, err
	}
	return yt.VideoThumbnails(ctx, ids)
}

func (u *Manager) prepareEpisodeArtwork(ctx context.Context, f *model.Feed, cfg *feed.Config) {
	if cfg.Custom.EpisodeCoverArt != "auto" || f.Provider != model.ProviderYoutube {
		return
	}
	var pending []*model.Episode
	var ids []string
	use := func(episode *model.Episode, name string) {
		episode.Thumbnail = strings.TrimRight(u.hostname, "/") + "/" + name
	}
	for _, episode := range f.Episodes {
		if episode.Status != model.EpisodeDownloaded {
			continue
		}
		name := episodeArtworkPath(cfg.ID, episode.ID)
		size, err := u.fs.Size(ctx, name)
		if err == nil && size > 0 {
			// A separate completion marker avoids reusing an interrupted or failed write (also on S3).
			if ready, err := u.fs.Size(ctx, name+".ready"); err == nil && ready == 1 {
				use(episode, name)
				continue
			}
		}
		pending = append(pending, episode)
		ids = append(ids, episode.ID)
	}
	if len(pending) == 0 {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	candidates, err := u.lookupThumbnails(lookupCtx, ids)
	cancel()
	if err != nil {
		log.WithError(err).Warn("episode artwork metadata unavailable; keeping source thumbnails")
		return
	}
	generator := u.artworkGenerator
	if generator == nil {
		generator = artwork.New(nil)
	}
	for _, episode := range pending {
		name := episodeArtworkPath(cfg.ID, episode.ID)
		data, err := generator.Generate(ctx, candidates[episode.ID])
		if err != nil {
			log.WithError(err).WithField("episode_id", episode.ID).Warn("episode artwork unavailable; keeping source thumbnail")
			continue
		}
		// Invalidate any stale marker before replacing a missing/incomplete image.
		if err = u.fs.Delete(ctx, name+".ready"); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.WithError(err).Warn("could not invalidate artwork cache; keeping source thumbnail")
			continue
		}
		if _, err = u.fs.Create(ctx, name, bytes.NewReader(data)); err == nil {
			_, err = u.fs.Create(ctx, name+".ready", strings.NewReader("1"))
		}
		if err != nil {
			log.WithError(err).WithField("episode_id", episode.ID).Warn("episode artwork storage failed; keeping source thumbnail")
			// Do not let an incomplete write be treated as a cached success on the next update.
			if cleanupErr := u.fs.Delete(ctx, name+".ready"); cleanupErr != nil {
				log.WithError(cleanupErr).Debug("could not remove artwork completion marker")
			}
			continue
		}
		// f is freshly loaded for this build; the original database thumbnail remains unchanged.
		use(episode, name)
	}
}
