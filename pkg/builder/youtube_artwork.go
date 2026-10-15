package builder

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/api/youtube/v3"
)

// VideoThumbnails retrieves current metadata, including episodes absent from the latest playlist page.
// Candidates are sorted by declared pixel area; missing dimensions use the API's size tiers.
func (yt *YouTubeBuilder) VideoThumbnails(ctx context.Context, ids []string) (map[string][]string, error) {
	result := make(map[string][]string)
	for start := 0; start < len(ids); start += maxYoutubeResults {
		end := min(start+maxYoutubeResults, len(ids))
		response, err := yt.client.Videos.List([]string{"snippet"}).Id(strings.Join(ids[start:end], ",")).Context(ctx).Do(yt.key)
		if err != nil {
			return nil, err
		}
		for _, video := range response.Items {
			if video.Snippet == nil {
				continue
			}
			result[video.Id] = thumbnailCandidates(video.Snippet.Thumbnails)
		}
	}
	return result, nil
}

func thumbnailCandidates(details *youtube.ThumbnailDetails) []string {
	if details == nil {
		return nil
	}
	type candidate struct {
		url  string
		area int64
	}
	var candidates []candidate
	for _, entry := range []struct {
		thumbnail     *youtube.Thumbnail
		width, height int64
	}{
		{details.Maxres, 1280, 720}, {details.Standard, 640, 480}, {details.High, 480, 360}, {details.Medium, 320, 180}, {details.Default, 120, 90},
	} {
		if entry.thumbnail == nil || entry.thumbnail.Url == "" {
			continue
		}
		width, height := entry.thumbnail.Width, entry.thumbnail.Height
		if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
			width, height = entry.width, entry.height
		}
		candidates = append(candidates, candidate{entry.thumbnail.Url, width * height})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].area > candidates[j].area })
	urls := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if !seen[candidate.url] {
			urls = append(urls, candidate.url)
			seen[candidate.url] = true
		}
	}
	return urls
}
