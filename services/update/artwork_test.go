package update

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/artwork"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/services/web"
)

type artworkTransport func(*http.Request) (*http.Response, error)

func (f artworkTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingArtworkStorage struct {
	fs.Storage
	failSuffix string
}

func (s failingArtworkStorage) Create(ctx context.Context, name string, r io.Reader) (int64, error) {
	if strings.HasSuffix(name, s.failSuffix) {
		// Model a storage implementation that leaves partial bytes behind on failure.
		_, _ = s.Storage.Create(ctx, name, strings.NewReader("partial"))
		return 0, fmt.Errorf("storage failed")
	}
	return s.Storage.Create(ctx, name, r)
}

func artworkFixture(t *testing.T) (*Manager, *feed.Config, *fs.Local) {
	t.Helper()
	storage, err := db.NewBadger(&db.Config{Dir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, storage.Close()) })
	files, err := fs.NewLocal(t.TempDir(), false, false)
	require.NoError(t, err)
	cfg := &feed.Config{ID: "example", Format: model.FormatVideo, Custom: feed.Custom{EpisodeCoverArt: "auto"}}
	require.NoError(t, storage.AddFeed(context.Background(), cfg.ID, &model.Feed{Provider: model.ProviderYoutube, Format: model.FormatVideo, Episodes: []*model.Episode{
		{ID: "existing", Title: "Existing", Thumbnail: "https://example.com/original.jpg", VideoURL: "https://youtube.com/watch?v=existing", Status: model.EpisodeDownloaded},
		{ID: "pending", Title: "Pending", Status: model.EpisodeNew},
	}}))
	return &Manager{db: storage, fs: files, hostname: "https://podcast.example.com/podcasts"}, cfg, files
}

func readArtworkRSS(t *testing.T, files fs.Storage) (string, string, string) {
	t.Helper()
	file, err := files.Open("example.xml")
	require.NoError(t, err)
	defer file.Close()
	var rss struct {
		Items []struct {
			GUID  string `xml:"guid"`
			Image struct {
				Href string `xml:"href,attr"`
			} `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
			Enclosure struct {
				URL string `xml:"url,attr"`
			} `xml:"enclosure"`
		} `xml:"channel>item"`
	}
	require.NoError(t, xml.NewDecoder(file).Decode(&rss))
	require.Len(t, rss.Items, 1)
	return rss.Items[0].Image.Href, rss.Items[0].GUID, rss.Items[0].Enclosure.URL
}

func TestAutomaticArtworkRefreshAndHTTP(t *testing.T) {
	ctx := context.Background()
	u, cfg, files := artworkFixture(t)
	var metadataCalls, imageCalls int
	u.thumbnailLookup = func(_ context.Context, ids []string) (map[string][]string, error) {
		metadataCalls++
		assert.Equal(t, []string{"existing"}, ids)
		return map[string][]string{"existing": {"https://i.ytimg.com/max", "https://i.ytimg.com/standard"}}, nil
	}
	var original bytes.Buffer
	require.NoError(t, png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 640, 360))))
	u.artworkGenerator = artwork.New(&http.Client{Transport: artworkTransport(func(r *http.Request) (*http.Response, error) {
		imageCalls++
		status, body := 200, original.Bytes()
		if r.URL.Path == "/max" {
			status, body = 404, nil
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})})
	require.NoError(t, u.buildXML(ctx, cfg))
	href, guid, enclosure := readArtworkRSS(t, files)
	assert.Equal(t, u.hostname+"/"+episodeArtworkPath(cfg.ID, "existing"), href)
	assert.Equal(t, "existing", guid)
	assert.Equal(t, u.hostname+"/example/existing.mp4", enclosure)
	server := httptest.NewServer(web.New(web.Config{Path: "podcasts"}, files, u.db).Handler)
	defer server.Close()
	parsed, err := url.Parse(href)
	require.NoError(t, err)
	resp, err := http.Get(server.URL + parsed.Path)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
	img, err := jpeg.Decode(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 1400, 1400), img.Bounds())
	// Repeated refreshes and re-enabling auto must reuse a completed persistent cache.
	for _, setting := range []string{"auto", "https://example.com/fixed.jpg", "", "auto"} {
		cfg.Custom.EpisodeCoverArt = setting
		require.NoError(t, u.buildXML(ctx, cfg))
		got, _, _ := readArtworkRSS(t, files)
		want := setting
		if setting == "" {
			want = "https://example.com/original.jpg"
		}
		if setting == "auto" {
			want = href
		}
		assert.Equal(t, want, got)
	}
	assert.Equal(t, 1, metadataCalls)
	assert.Equal(t, 2, imageCalls)
	// Removing a cached image regenerates it, even if a stale completion marker remains.
	require.NoError(t, files.Delete(ctx, episodeArtworkPath(cfg.ID, "existing")))
	require.NoError(t, u.buildXML(ctx, cfg))
	assert.Equal(t, 2, metadataCalls)
	assert.Equal(t, 4, imageCalls)
	ep, err := u.db.GetEpisode(ctx, cfg.ID, "existing")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/original.jpg", ep.Thumbnail)
	assert.Equal(t, model.EpisodeDownloaded, ep.Status)
}

func TestArtworkModeSelection(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		provider      model.Provider
	}{
		{"fixed URL", "https://example.com/fixed.png", model.ProviderYoutube},
		{"disabled", "", model.ProviderYoutube},
		{"non YouTube auto", "auto", model.ProviderVimeo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, cfg, files := artworkFixture(t)
			cfg.Custom.EpisodeCoverArt = tc.setting
			require.NoError(t, u.db.AddFeed(context.Background(), cfg.ID, &model.Feed{Provider: tc.provider, Format: model.FormatVideo}))
			u.thumbnailLookup = func(context.Context, []string) (map[string][]string, error) {
				t.Fatal("unexpected metadata request")
				return nil, nil
			}
			require.NoError(t, u.buildXML(context.Background(), cfg))
			got, _, _ := readArtworkRSS(t, files)
			want := "https://example.com/original.jpg"
			if tc.setting != "" && tc.setting != "auto" {
				want = tc.setting
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestArtworkFailureFallbackAndRetry(t *testing.T) {
	for _, failure := range []string{"metadata", "missing metadata", "image", "image write", "marker write"} {
		t.Run(failure, func(t *testing.T) {
			u, cfg, files := artworkFixture(t)
			u.thumbnailLookup = func(context.Context, []string) (map[string][]string, error) {
				if failure == "metadata" {
					return nil, fmt.Errorf("API unavailable")
				}
				if failure == "missing metadata" {
					return nil, nil
				}
				return map[string][]string{"existing": {"https://i.ytimg.com/source"}}, nil
			}
			var encoded bytes.Buffer
			require.NoError(t, png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 4, 2))))
			u.artworkGenerator = artwork.New(&http.Client{Transport: artworkTransport(func(r *http.Request) (*http.Response, error) {
				status := 200
				if failure == "image" {
					status = 404
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(encoded.Bytes())), Header: make(http.Header), Request: r}, nil
			})})
			if failure == "image write" {
				u.fs = failingArtworkStorage{Storage: files, failSuffix: ".jpg"}
			}
			if failure == "marker write" {
				u.fs = failingArtworkStorage{Storage: files, failSuffix: ".ready"}
			}
			require.NoError(t, u.buildXML(context.Background(), cfg))
			got, _, _ := readArtworkRSS(t, files)
			assert.Equal(t, "https://example.com/original.jpg", got)
			u.fs = files
			u.thumbnailLookup = func(context.Context, []string) (map[string][]string, error) {
				return map[string][]string{"existing": {"https://i.ytimg.com/source"}}, nil
			}
			failure = "recovered"
			require.NoError(t, u.buildXML(context.Background(), cfg))
			got, _, _ = readArtworkRSS(t, files)
			assert.Equal(t, u.hostname+"/"+episodeArtworkPath(cfg.ID, "existing"), got)
		})
	}
}

func TestArtworkPathSafety(t *testing.T) {
	name := episodeArtworkPath("../../feed?#", "../episode.jpg?x=y#z")
	assert.Regexp(t, `^artwork/square-blur-dark30-v1/[a-f0-9]{64}/[a-f0-9]{64}\.jpg$`, name)
	assert.NotEqual(t, name, episodeArtworkPath("../../feed?#", "different"))
	assert.NotEqual(t, name, episodeArtworkPath("different", "../episode.jpg?x=y#z"))
}

func TestArtworkMissingKeyFallback(t *testing.T) {
	u, cfg, files := artworkFixture(t)
	require.NoError(t, u.buildXML(context.Background(), cfg))
	got, _, _ := readArtworkRSS(t, files)
	assert.Equal(t, "https://example.com/original.jpg", got)
}
