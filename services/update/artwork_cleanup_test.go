package update

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
)

func putArtworkObjects(t *testing.T, storage fs.Storage, feedID, episodeID string) []string {
	t.Helper()
	var names []string
	for _, version := range artworkVersions {
		name := strings.Replace(episodeArtworkPath(feedID, episodeID), "/"+artworkVersion+"/", "/"+version+"/", 1)
		for _, object := range []string{name, name + ".ready"} {
			_, err := storage.Create(context.Background(), object, strings.NewReader("1"))
			require.NoError(t, err)
			names = append(names, object)
		}
	}
	return names
}
func assertArtworkMissing(t *testing.T, storage fs.Storage, names []string) {
	t.Helper()
	for _, name := range names {
		_, err := storage.Size(context.Background(), name)
		assert.True(t, errors.Is(err, os.ErrNotExist), name)
	}
}

func TestCleanupRemovesEpisodeArtwork(t *testing.T) {
	for _, missingMedia := range []bool{false, true} {
		t.Run(map[bool]string{true: "missing media", false: "existing media"}[missingMedia], func(t *testing.T) {
			u, cfg, files := artworkFixture(t)
			cfg.Clean = &feed.Cleanup{KeepLast: 1}
			u.feeds = map[string]*feed.Config{cfg.ID: cfg}
			ctx := context.Background()
			require.NoError(t, u.db.UpdateEpisode(cfg.ID, "existing", func(e *model.Episode) error { e.PubDate = time.Unix(1, 0); return nil }))
			require.NoError(t, u.db.AddFeed(ctx, cfg.ID, &model.Feed{Episodes: []*model.Episode{{ID: "newest", Title: "New", Status: model.EpisodeDownloaded, PubDate: time.Unix(2, 0)}}}))
			old := putArtworkObjects(t, files, cfg.ID, "existing")
			kept := putArtworkObjects(t, files, cfg.ID, "newest")
			if !missingMedia {
				_, err := files.Create(ctx, "example/existing.mp4", strings.NewReader("video"))
				require.NoError(t, err)
			}
			_, err := files.Create(ctx, "example/newest.mp4", strings.NewReader("video"))
			require.NoError(t, err)
			require.NoError(t, u.cleanup(ctx, cfg))
			assertArtworkMissing(t, files, old)
			for _, name := range kept {
				_, err := files.Size(ctx, name)
				require.NoError(t, err)
			}
			episode, err := u.db.GetEpisode(ctx, cfg.ID, "existing")
			require.NoError(t, err)
			assert.Equal(t, model.EpisodeCleaned, episode.Status)
		})
	}
}

type artworkDeleteFailure struct {
	fs.Storage
	fail string
}

func (s artworkDeleteFailure) Delete(ctx context.Context, name string) error {
	if name == s.fail {
		return os.ErrPermission
	}
	return s.Storage.Delete(ctx, name)
}
func TestArtworkCleanupFailureRetries(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	cfg.Clean = &feed.Cleanup{KeepLast: 1}
	u.feeds = map[string]*feed.Config{cfg.ID: cfg}
	ctx := context.Background()
	require.NoError(t, u.db.AddFeed(ctx, cfg.ID, &model.Feed{Episodes: []*model.Episode{{ID: "newest", Status: model.EpisodeDownloaded, PubDate: time.Now()}}}))
	names := putArtworkObjects(t, files, cfg.ID, "existing")
	target := episodeArtworkPath(cfg.ID, "existing")
	u.fs = artworkDeleteFailure{Storage: files, fail: target}
	require.Error(t, u.cleanup(ctx, cfg))
	_, err := files.Size(ctx, target)
	require.NoError(t, err)
	u.fs = files
	require.NoError(t, u.PruneArtwork(ctx))
	assertArtworkMissing(t, files, names)
}

func TestArtworkReconciliationOwnershipAndDeletion(t *testing.T) {
	for _, kind := range []string{"cleaned", "deleted episode", "deleted feed", "removed config", "missing video"} {
		t.Run(kind, func(t *testing.T) {
			u, cfg, files := artworkFixture(t)
			u.feeds = map[string]*feed.Config{cfg.ID: cfg}
			ctx := context.Background()
			_, err := files.Create(ctx, "example/existing.mp4", strings.NewReader("video"))
			require.NoError(t, err)
			names := putArtworkObjects(t, files, cfg.ID, "existing")
			// Unrelated files, unknown future cache versions and a user image are never reclaimed.
			unrelated := []string{"artwork/user.jpg", "artwork/unknown/future.jpg", "artwork/" + artworkVersion + "/manual.jpg", "example/user-cover.jpg"}
			for _, name := range unrelated {
				_, err := files.Create(ctx, name, strings.NewReader("user"))
				require.NoError(t, err)
			}
			require.NoError(t, u.PruneArtwork(ctx))
			for _, name := range names {
				_, err := files.Size(ctx, name)
				require.NoError(t, err)
			}
			switch kind {
			case "cleaned":
				require.NoError(t, u.db.UpdateEpisode(cfg.ID, "existing", func(e *model.Episode) error { e.Status = model.EpisodeCleaned; return nil }))
			case "deleted episode":
				require.NoError(t, u.db.DeleteEpisode(cfg.ID, "existing"))
			case "deleted feed":
				require.NoError(t, u.db.DeleteFeed(ctx, cfg.ID))
			case "removed config":
				u.feeds = map[string]*feed.Config{}
			case "missing video":
				require.NoError(t, files.Delete(ctx, "example/existing.mp4"))
			}
			require.NoError(t, u.PruneArtwork(ctx))
			assertArtworkMissing(t, files, names)
			for _, name := range unrelated {
				_, err := files.Size(ctx, name)
				require.NoError(t, err)
			}
		})
	}
}

func TestLegacyArtworkRetiredAfterRSSAndFixedReferencesProtected(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	u.feeds = map[string]*feed.Config{cfg.ID: cfg}
	ctx := context.Background()
	_, err := files.Create(ctx, "example/existing.mp4", strings.NewReader("video"))
	require.NoError(t, err)
	names := putArtworkObjects(t, files, cfg.ID, "existing")
	require.NoError(t, u.PruneArtwork(ctx)) // Keep old URLs before successfully publishing replacement RSS.
	cfg.Custom.EpisodeCoverArt = strings.Replace(u.hostname, "podcast.example.com", "PODCAST.EXAMPLE.COM:443", 1) + "/" + names[0]
	require.NoError(t, u.buildXML(ctx, cfg))
	for _, name := range names {
		_, err := files.Size(ctx, name)
		if strings.Contains(name, "/"+artworkVersion+"/") || name == names[0] || name == names[1] {
			require.NoError(t, err)
		} else {
			assert.True(t, errors.Is(err, os.ErrNotExist), name)
		}
	}
	require.NoError(t, files.Delete(ctx, "example/existing.mp4"))
	require.NoError(t, u.PruneArtwork(ctx))
	_, err = files.Size(ctx, names[0])
	require.NoError(t, err) // Explicit fixed URL wins over reclamation.
}

func TestPartialArtworkReclaimed(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	u.feeds = map[string]*feed.Config{cfg.ID: cfg}
	ctx := context.Background()
	_, err := files.Create(ctx, "example/existing.mp4", strings.NewReader("video"))
	require.NoError(t, err)
	name := episodeArtworkPath(cfg.ID, "existing")
	_, err = files.Create(ctx, name, strings.NewReader("incomplete"))
	require.NoError(t, err)
	require.NoError(t, u.PruneArtwork(ctx))
	assertArtworkMissing(t, files, []string{name})
}

type artworkDBFailure struct{ db.Storage }

func (s artworkDBFailure) GetFeed(context.Context, string) (*model.Feed, error) {
	return nil, os.ErrPermission
}
func TestArtworkDatabaseFailureCannotDelete(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	u.feeds = map[string]*feed.Config{cfg.ID: cfg}
	names := putArtworkObjects(t, files, cfg.ID, "existing")
	u.db = artworkDBFailure{Storage: u.db}
	require.Error(t, u.PruneArtwork(context.Background()))
	for _, name := range names {
		_, err := files.Size(context.Background(), name)
		require.NoError(t, err)
	}
}

func TestFailedRSSPreservesLegacyArtwork(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	u.feeds = map[string]*feed.Config{cfg.ID: cfg}
	cfg.Custom.EpisodeCoverArt = "https://example.com/fixed.jpg"
	names := putArtworkObjects(t, files, cfg.ID, "existing")
	u.fs = failingArtworkStorage{Storage: files, failSuffix: ".xml"}
	require.Error(t, u.buildXML(context.Background(), cfg))
	for _, name := range names {
		_, err := files.Size(context.Background(), name)
		require.NoError(t, err)
	}
}

func TestFailedVideoDeletePreservesArtwork(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	cfg.Clean = &feed.Cleanup{KeepLast: 1}
	ctx := context.Background()
	require.NoError(t, u.db.AddFeed(ctx, cfg.ID, &model.Feed{Episodes: []*model.Episode{{ID: "newest", Status: model.EpisodeDownloaded, PubDate: time.Now()}}}))
	_, err := files.Create(ctx, "example/existing.mp4", strings.NewReader("video"))
	require.NoError(t, err)
	names := putArtworkObjects(t, files, cfg.ID, "existing")
	u.fs = artworkDeleteFailure{Storage: files, fail: "example/existing.mp4"}
	require.Error(t, u.cleanup(ctx, cfg))
	for _, name := range names {
		_, err := files.Size(ctx, name)
		require.NoError(t, err)
	}
	ep, err := u.db.GetEpisode(ctx, cfg.ID, "existing")
	require.NoError(t, err)
	assert.Equal(t, model.EpisodeDownloaded, ep.Status)
}

func TestFailedGenerationCleanupPreservesOtherVersions(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	names := putArtworkObjects(t, files, cfg.ID, "existing")
	require.NoError(t, u.deleteArtworkObjects(context.Background(), episodeArtworkPath(cfg.ID, "existing")))
	for _, name := range names {
		_, err := files.Size(context.Background(), name)
		if strings.Contains(name, "/"+artworkVersion+"/") {
			assert.True(t, errors.Is(err, os.ErrNotExist))
		} else {
			require.NoError(t, err)
		}
	}
}
