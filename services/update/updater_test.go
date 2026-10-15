package update

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
)

// Regenerating XML must update already downloaded episodes without changing stored metadata.
func TestBuildXMLRefreshEpisodeCoverArt(t *testing.T) {
	ctx := context.Background()
	storage, err := db.NewBadger(&db.Config{Dir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, storage.Close()) })
	dataDir := t.TempDir()
	files, err := fs.NewLocal(dataDir, false, false)
	require.NoError(t, err)
	cfg := &feed.Config{ID: "example", Format: model.FormatVideo}
	const thumbnail = "https://example.com/default_live.jpg"
	require.NoError(t, storage.AddFeed(ctx, cfg.ID, &model.Feed{
		Episodes: []*model.Episode{{ID: "existing", Title: "Existing", Status: model.EpisodeDownloaded, Thumbnail: thumbnail}},
	}))
	u := &Manager{db: storage, fs: files, hostname: "https://podcast.example.com"}
	for _, cover := range []string{"", "https://example.com/cover.png", ""} {
		cfg.Custom.EpisodeCoverArt = cover
		require.NoError(t, u.buildXML(ctx, cfg))
		data, err := os.ReadFile(filepath.Join(dataDir, "example.xml"))
		require.NoError(t, err)
		var rss struct {
			Items []struct {
				GUID  string `xml:"guid"`
				Image struct {
					Href string `xml:"href,attr"`
				} `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
			} `xml:"channel>item"`
		}
		require.NoError(t, xml.Unmarshal(data, &rss))
		require.Len(t, rss.Items, 1)
		assert.Equal(t, "existing", rss.Items[0].GUID)
		want := cover
		if want == "" {
			want = thumbnail
		}
		assert.Equal(t, want, rss.Items[0].Image.Href)
		episode, err := storage.GetEpisode(ctx, cfg.ID, "existing")
		require.NoError(t, err)
		assert.Equal(t, thumbnail, episode.Thumbnail)
		assert.Equal(t, model.EpisodeDownloaded, episode.Status)
	}
}
