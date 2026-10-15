package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

func TestThumbnailCandidates(t *testing.T) {
	assert.Empty(t, thumbnailCandidates(nil))
	assert.Equal(t, []string{"standard", "high", "medium", "default"}, thumbnailCandidates(&youtube.ThumbnailDetails{
		Standard: &youtube.Thumbnail{Url: "standard"}, High: &youtube.Thumbnail{Url: "high"}, Medium: &youtube.Thumbnail{Url: "medium"}, Default: &youtube.Thumbnail{Url: "default"},
	}))
	assert.Equal(t, []string{"actually-larger", "max"}, thumbnailCandidates(&youtube.ThumbnailDetails{
		Maxres: &youtube.Thumbnail{Url: "max", Width: 640, Height: 360}, Standard: &youtube.Thumbnail{Url: "actually-larger", Width: 1280, Height: 720}, High: &youtube.Thumbnail{Url: "max"},
	}))
	assert.Empty(t, thumbnailCandidates(&youtube.ThumbnailDetails{Maxres: &youtube.Thumbnail{}}))
}

func TestVideoThumbnailsBatching(t *testing.T) {
	var batches []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "snippet", r.URL.Query().Get("part"))
		ids := strings.Split(r.URL.Query().Get("id"), ",")
		batches = append(batches, len(ids))
		items := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			items = append(items, map[string]any{"id": id, "snippet": map[string]any{"thumbnails": map[string]any{"high": map[string]any{"url": "https://i.ytimg.com/" + id + ".jpg"}}}})
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"items": items}))
	}))
	defer srv.Close()
	client, err := youtube.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	yt := &YouTubeBuilder{client: client, key: "key"}
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	result, err := yt.VideoThumbnails(context.Background(), ids)
	require.NoError(t, err)
	assert.Equal(t, []int{50, 1}, batches)
	assert.Len(t, result, 51)
	assert.Equal(t, []string{"https://i.ytimg.com/50.jpg"}, result["50"])
}
