package update

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

type mediaDownloader struct{ calls int }

func (d *mediaDownloader) Download(context.Context, *feed.Config, *model.Episode) (io.ReadCloser, error) {
	d.calls++
	return io.NopCloser(strings.NewReader("downloaded video")), nil
}
func (*mediaDownloader) PlaylistMetadata(context.Context, string) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, nil
}
func mediaFixture(t *testing.T, episodes ...*model.Episode) (*Manager, *feed.Config, string, *mediaDownloader) {
	t.Helper()
	database, err := db.NewBadger(&db.Config{Dir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	root := t.TempDir()
	files, err := fs.NewLocal(root, false, false)
	require.NoError(t, err)
	cfg := &feed.Config{ID: "example", Format: model.FormatVideo, PageSize: 50}
	for _, ep := range episodes {
		ep.Title = "Example episode"
		ep.VideoURL = "https://example.com/video"
	}
	require.NoError(t, database.AddFeed(context.Background(), cfg.ID, &model.Feed{Provider: model.ProviderYoutube, Format: model.FormatVideo, Episodes: episodes}))
	dl := &mediaDownloader{}
	return &Manager{db: database, fs: files, downloader: dl, hostname: "https://podcast.example.com", feeds: map[string]*feed.Config{cfg.ID: cfg}}, cfg, root, dl
}
func putMedia(t *testing.T, u *Manager, name string) {
	t.Helper()
	_, err := u.fs.Create(context.Background(), "example/"+name, strings.NewReader("existing video"))
	require.NoError(t, err)
}

func TestMediaRefreshExistingAndNew(t *testing.T) {
	for _, tc := range []struct {
		name, existing, enclosure string
		status                    model.EpisodeStatus
		downloads                 int
	}{
		{"new", "", "-synthetic-.mp4", model.EpisodeNew, 1},
		{"original", "-synthetic-.mp4", "-synthetic-.mp4", model.EpisodeDownloaded, 0},
		{"beta alias", "synthetic.mp4", "-synthetic-.mp4", model.EpisodeDownloaded, 0},
		{"new status existing alias", "synthetic.mp4", "-synthetic-.mp4", model.EpisodeNew, 0},
		{"missing downloaded", "", "-synthetic-.mp4", model.EpisodeDownloaded, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ep := &model.Episode{ID: "-synthetic-", Status: tc.status}
			u, cfg, root, dl := mediaFixture(t, ep)
			if tc.existing != "" {
				putMedia(t, u, tc.existing)
			}
			queue, err := u.fetchEpisodes(ctx, cfg)
			require.NoError(t, err)
			require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
			assert.Equal(t, tc.downloads, dl.calls)
			require.NoError(t, u.buildXML(ctx, cfg))
			_, guid, enclosure := readArtworkRSS(t, u.fs)
			assert.Equal(t, ep.ID, guid)
			assert.Equal(t, u.hostname+"/example/"+tc.enclosure, enclosure)
			_, err = u.fs.Size(ctx, "example/"+tc.enclosure)
			require.NoError(t, err)
			stored, err := u.db.GetEpisode(ctx, cfg.ID, ep.ID)
			require.NoError(t, err)
			assert.Equal(t, model.EpisodeDownloaded, stored.Status)
			assert.Empty(t, stored.MediaFilename)
			if tc.existing == "synthetic.mp4" {
				original, err := os.Stat(filepath.Join(root, "example", tc.existing))
				require.NoError(t, err)
				migrated, err := os.Stat(filepath.Join(root, "example", tc.enclosure))
				require.NoError(t, err)
				assert.True(t, os.SameFile(original, migrated), "migration must not duplicate bytes")
				require.NoError(t, u.buildXML(ctx, cfg), "repeat update is idempotent")
			}
		})
	}
}

func TestMediaCanonicalPreferredAndAmbiguousAliasRejected(t *testing.T) {
	ctx := context.Background()
	a := &model.Episode{ID: "-synthetic-", Status: model.EpisodeNew}
	b := &model.Episode{ID: "_synthetic_", Status: model.EpisodeDownloaded}
	u, cfg, _, dl := mediaFixture(t, a, b)
	putMedia(t, u, "synthetic.mp4")
	stored, err := u.db.GetFeed(ctx, cfg.ID)
	require.NoError(t, err)
	_, _, err = u.resolveMedia(ctx, cfg, a, claimMedia(cfg, stored.Episodes))
	assert.ErrorIs(t, err, os.ErrNotExist)
	queue, err := u.fetchEpisodes(ctx, cfg)
	require.NoError(t, err)
	require.Len(t, queue, 2)
	require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
	assert.Equal(t, 2, dl.calls)
	name, _, err := u.resolveMedia(ctx, cfg, a, claimMedia(cfg, stored.Episodes))
	require.NoError(t, err)
	assert.Equal(t, "-synthetic-.mp4", name)
}

func TestMediaCleanupHardlinkAliasesAndArtwork(t *testing.T) {
	ctx := context.Background()
	old := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded, PubDate: time.Unix(1, 0)}
	newest := &model.Episode{ID: "newest", Status: model.EpisodeDownloaded, PubDate: time.Unix(2, 0)}
	u, cfg, root, _ := mediaFixture(t, old, newest)
	cfg.Clean = &feed.Cleanup{KeepLast: 1}
	cfg.Custom.EpisodeCoverArt = "auto"
	putMedia(t, u, "synthetic.mp4")
	covers := putArtworkObjects(t, u.fs, cfg.ID, old.ID)
	require.NoError(t, u.PruneArtwork(ctx))
	_, err := u.fs.Size(ctx, episodeArtworkPath(cfg.ID, old.ID))
	require.NoError(t, err, "alias-only media protects current artwork")
	canonical := filepath.Join(root, "example", "-synthetic-.mp4")
	alias := filepath.Join(root, "example", "synthetic.mp4")
	require.NoError(t, os.Link(alias, canonical))
	first, err := os.Stat(alias)
	require.NoError(t, err)
	second, err := os.Stat(canonical)
	require.NoError(t, err)
	require.True(t, os.SameFile(first, second))
	putMedia(t, u, "newest.mp4")
	require.NoError(t, u.cleanup(ctx, cfg))
	for _, name := range []string{alias, canonical} {
		_, err := os.Stat(name)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
	assertArtworkMissing(t, u.fs, covers)
	_, err = u.fs.Size(ctx, "example/newest.mp4")
	require.NoError(t, err)
}

func TestMediaCleanupProtectsRetainedAlias(t *testing.T) {
	ctx := context.Background()
	old := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded, PubDate: time.Unix(1, 0)}
	kept := &model.Episode{ID: "synthetic", Status: model.EpisodeDownloaded, PubDate: time.Unix(2, 0)}
	u, cfg, _, _ := mediaFixture(t, old, kept)
	cfg.Clean = &feed.Cleanup{KeepLast: 1}
	putMedia(t, u, "-synthetic-.mp4")
	putMedia(t, u, "synthetic.mp4")
	require.NoError(t, u.cleanup(ctx, cfg))
	_, err := u.fs.Size(ctx, "example/synthetic.mp4")
	require.NoError(t, err)
	_, err = u.fs.Size(ctx, "example/-synthetic-.mp4")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestMediaCleanupAliasFailureRetainsStateAndArtwork(t *testing.T) {
	ctx := context.Background()
	old := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded, PubDate: time.Unix(1, 0)}
	newest := &model.Episode{ID: "newest", Status: model.EpisodeDownloaded, PubDate: time.Unix(2, 0)}
	u, cfg, _, _ := mediaFixture(t, old, newest)
	cfg.Clean = &feed.Cleanup{KeepLast: 1}
	putMedia(t, u, "-synthetic-.mp4")
	putMedia(t, u, "synthetic.mp4")
	covers := putArtworkObjects(t, u.fs, cfg.ID, old.ID)
	files := u.fs
	u.fs = artworkDeleteFailure{Storage: files, fail: "example/synthetic.mp4"}
	require.Error(t, u.cleanup(ctx, cfg))
	stored, err := u.db.GetEpisode(ctx, cfg.ID, old.ID)
	require.NoError(t, err)
	assert.Equal(t, model.EpisodeDownloaded, stored.Status)
	for _, name := range covers {
		_, err := files.Size(ctx, name)
		require.NoError(t, err)
	}
	u.fs = files
	require.NoError(t, u.cleanup(ctx, cfg))
	assertArtworkMissing(t, files, covers)
}

// Embedding only Storage models a backend without hard-link support, such as S3.
func TestMediaCompatibilityWithoutHardlinks(t *testing.T) {
	ctx := context.Background()
	u, cfg, _, dl := mediaFixture(t, &model.Episode{ID: "_synthetic_", Status: model.EpisodeDownloaded})
	putMedia(t, u, "synthetic.mp4")
	u.fs = struct{ fs.Storage }{u.fs}
	queue, err := u.fetchEpisodes(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
	assert.Zero(t, dl.calls)
	require.NoError(t, u.buildXML(ctx, cfg))
	_, guid, enclosure := readArtworkRSS(t, u.fs)
	assert.Equal(t, "_synthetic_", guid)
	assert.Equal(t, u.hostname+"/example/synthetic.mp4", enclosure)
}

func TestMediaMigrationKeepsExistingCanonicalContent(t *testing.T) {
	ctx := context.Background()
	ep := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded}
	u, cfg, _, _ := mediaFixture(t, ep)
	putMedia(t, u, "synthetic.mp4")
	_, err := u.fs.Create(ctx, "example/-synthetic-.mp4", strings.NewReader("canonical content"))
	require.NoError(t, err)
	name, _, err := u.resolveMedia(ctx, cfg, ep, claimMedia(cfg, []*model.Episode{ep}))
	require.NoError(t, err)
	assert.Equal(t, "-synthetic-.mp4", name)
	file, err := u.fs.Open("example/" + name)
	require.NoError(t, err)
	defer file.Close()
	contents, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, "canonical content", string(contents))
}

type unavailableMediaLinks struct{ fs.Storage }

func (unavailableMediaLinks) LinkMedia(context.Context, string, string) error {
	return os.ErrPermission
}

func TestMediaMigrationFailureRetainsWorkingURL(t *testing.T) {
	ctx := context.Background()
	u, cfg, _, dl := mediaFixture(t, &model.Episode{ID: "_synthetic_", Status: model.EpisodeDownloaded})
	putMedia(t, u, "synthetic.mp4")
	u.fs = unavailableMediaLinks{u.fs}
	queue, err := u.fetchEpisodes(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
	assert.Zero(t, dl.calls)
	require.NoError(t, u.buildXML(ctx, cfg))
	_, guid, enclosure := readArtworkRSS(t, u.fs)
	assert.Equal(t, "_synthetic_", guid)
	assert.Equal(t, u.hostname+"/example/synthetic.mp4", enclosure)
	_, err = u.fs.Size(ctx, "example/synthetic.mp4")
	require.NoError(t, err)
}

func TestMediaCustomTemplateMigration(t *testing.T) {
	for _, source := range []string{"-synthetic-.mp4", "synthetic.mp4"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			ep := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded}
			u, cfg, root, dl := mediaFixture(t, ep)
			cfg.FilenameTemplate = "{{pub_date}}-{{title}}-{{id}}"
			target := feed.EpisodeName(cfg, ep)
			putMedia(t, u, source)
			queue, err := u.fetchEpisodes(ctx, cfg)
			require.NoError(t, err)
			require.Empty(t, queue)
			require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
			assert.Zero(t, dl.calls)
			require.NoError(t, u.buildXML(ctx, cfg))
			_, guid, enclosure := readArtworkRSS(t, u.fs)
			assert.Equal(t, ep.ID, guid)
			assert.Equal(t, u.hostname+"/example/"+target, enclosure)
			old, err := os.Stat(filepath.Join(root, "example", source))
			require.NoError(t, err)
			current, err := os.Stat(filepath.Join(root, "example", target))
			require.NoError(t, err)
			assert.True(t, os.SameFile(old, current))
			require.NoError(t, u.buildXML(ctx, cfg))
		})
	}
}

func TestMediaCustomTemplateCollisionUsesOriginalIDs(t *testing.T) {
	ctx := context.Background()
	a := &model.Episode{ID: "first", Status: model.EpisodeNew}
	b := &model.Episode{ID: "second", Status: model.EpisodeNew}
	u, cfg, _, dl := mediaFixture(t, a, b)
	cfg.FilenameTemplate = "{{title}}"
	target := feed.EpisodeName(cfg, a)
	_, err := u.fs.Create(ctx, "example/"+target, strings.NewReader("unclaimed shared content"))
	require.NoError(t, err)
	queue, err := u.fetchEpisodes(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
	assert.Equal(t, 2, dl.calls)
	for _, ep := range []*model.Episode{a, b} {
		stored, err := u.db.GetFeed(ctx, cfg.ID)
		require.NoError(t, err)
		name, _, err := u.resolveMedia(ctx, cfg, ep, claimMedia(cfg, stored.Episodes))
		require.NoError(t, err)
		assert.Equal(t, ep.ID+".mp4", name)
	}
	file, err := u.fs.Open("example/" + target)
	require.NoError(t, err)
	defer file.Close()
	contents, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, "unclaimed shared content", string(contents))
	require.NoError(t, u.buildXML(ctx, cfg))
	rss, err := u.fs.Open("example.xml")
	require.NoError(t, err)
	defer rss.Close()
	content, err := io.ReadAll(rss)
	require.NoError(t, err)
	assert.Contains(t, string(content), "/example/first.mp4")
	assert.Contains(t, string(content), "/example/second.mp4")
}

func TestMediaCustomTemplateCannotClaimAnotherOriginalID(t *testing.T) {
	ctx := context.Background()
	a := &model.Episode{ID: "first", Status: model.EpisodeDownloaded}
	b := &model.Episode{ID: "second", Status: model.EpisodeDownloaded}
	u, cfg, _, _ := mediaFixture(t, a, b)
	cfg.FilenameTemplate = "{{title}}"
	require.NoError(t, u.db.UpdateEpisode(cfg.ID, a.ID, func(ep *model.Episode) error { ep.Title = "second"; return nil }))
	require.NoError(t, u.db.UpdateEpisode(cfg.ID, b.ID, func(ep *model.Episode) error { ep.Title = "Other"; return nil }))
	putMedia(t, u, "first.mp4")
	putMedia(t, u, "second.mp4")
	stored, err := u.db.GetFeed(ctx, cfg.ID)
	require.NoError(t, err)
	claims := claimMedia(cfg, stored.Episodes)
	for _, ep := range stored.Episodes {
		name, _, err := u.resolveMedia(ctx, cfg, ep, claims)
		if ep.ID == "first" {
			require.NoError(t, err)
			assert.Equal(t, "first.mp4", name)
		} else {
			assert.ErrorIs(t, err, os.ErrNotExist)
		}
	}
	_, err = u.fs.Size(ctx, "example/Other.mp4")
	assert.ErrorIs(t, err, os.ErrNotExist, "ambiguous source must not be linked")
}

func TestMediaCleanedRecordStillBlocksAmbiguousHistoricalAlias(t *testing.T) {
	ctx := context.Background()
	old := &model.Episode{ID: "-synthetic-", Status: model.EpisodeCleaned}
	kept := &model.Episode{ID: "_synthetic_", Status: model.EpisodeDownloaded}
	u, cfg, _, dl := mediaFixture(t, old, kept)
	putMedia(t, u, "synthetic.mp4")
	queue, err := u.fetchEpisodes(ctx, cfg)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, kept.ID, queue[0].ID)
	require.NoError(t, u.downloadEpisodes(ctx, cfg, queue))
	assert.Equal(t, 1, dl.calls)
}

func TestMediaCustomTemplateCleanupAllKnownPaths(t *testing.T) {
	ctx := context.Background()
	old := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded, PubDate: time.Unix(1, 0)}
	kept := &model.Episode{ID: "newest", Status: model.EpisodeDownloaded, PubDate: time.Unix(2, 0)}
	u, cfg, _, _ := mediaFixture(t, old, kept)
	cfg.FilenameTemplate = "{{pub_date}}-{{id}}"
	cfg.Clean = &feed.Cleanup{KeepLast: 1}
	for _, name := range feed.EpisodeNameCandidates(cfg, old) {
		putMedia(t, u, name)
	}
	putMedia(t, u, feed.EpisodeName(cfg, kept))
	require.NoError(t, u.cleanup(ctx, cfg))
	for _, name := range feed.EpisodeNameCandidates(cfg, old) {
		_, err := u.fs.Size(ctx, "example/"+name)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
	_, err := u.fs.Size(ctx, "example/"+feed.EpisodeName(cfg, kept))
	require.NoError(t, err)
}

func TestMediaDirectRSSRefreshOmitsMissingMedia(t *testing.T) {
	ctx := context.Background()
	u, cfg, _, _ := mediaFixture(t, &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded})
	require.NoError(t, u.buildXML(ctx, cfg))
	rss, err := u.fs.Open("example.xml")
	require.NoError(t, err)
	defer rss.Close()
	content, err := io.ReadAll(rss)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "<enclosure")
}

func TestMediaCanonicalRestoresCompatibleOldURL(t *testing.T) {
	ctx := context.Background()
	ep := &model.Episode{ID: "-synthetic-", Status: model.EpisodeDownloaded}
	u, cfg, root, _ := mediaFixture(t, ep)
	putMedia(t, u, "-synthetic-.mp4")
	require.NoError(t, u.buildXML(ctx, cfg))
	canonical, err := os.Stat(filepath.Join(root, "example", "-synthetic-.mp4"))
	require.NoError(t, err)
	alias, err := os.Stat(filepath.Join(root, "example", "synthetic.mp4"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(canonical, alias))
	_, guid, enclosure := readArtworkRSS(t, u.fs)
	assert.Equal(t, ep.ID, guid)
	assert.Equal(t, u.hostname+"/example/-synthetic-.mp4", enclosure)
}
