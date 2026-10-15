package feed

import (
	"context"
	"encoding/xml"
	"testing"
	"time"

	itunes "github.com/eduncan911/podcast"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildXML(t *testing.T) {
	feed := model.Feed{
		Episodes: []*model.Episode{
			{
				ID:          "1",
				Status:      model.EpisodeDownloaded,
				Title:       "title",
				Description: "description",
			},
		},
	}

	cfg := Config{
		ID:     "test",
		Custom: Custom{Description: "description", Category: "Technology", Subcategories: []string{"Gadgets", "Podcasting"}},
	}

	out, err := Build(context.Background(), &feed, &cfg, "http://localhost/")
	assert.NoError(t, err)

	assert.EqualValues(t, "description", out.Description)
	assert.EqualValues(t, "Technology", out.Category)

	require.Len(t, out.ICategories, 1)
	category := out.ICategories[0]
	assert.EqualValues(t, "Technology", category.Text)

	require.Len(t, category.ICategories, 2)
	assert.EqualValues(t, "Gadgets", category.ICategories[0].Text)
	assert.EqualValues(t, "Podcasting", category.ICategories[1].Text)

	require.Len(t, out.Items, 1)
	require.NotNil(t, out.Items[0].Enclosure)
	assert.EqualValues(t, out.Items[0].Enclosure.URL, "http://localhost/test/1.mp4")
	assert.EqualValues(t, out.Items[0].Enclosure.Type, itunes.MP4)
}

func TestBuildXMLWithFilenameTemplate(t *testing.T) {
	feed := model.Feed{
		Episodes: []*model.Episode{
			{
				ID:          "video123",
				Status:      model.EpisodeDownloaded,
				Title:       "A title / with chars",
				Description: "description",
				PubDate:     time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
			},
		},
	}

	cfg := Config{
		ID:               "test",
		Format:           model.FormatVideo,
		FilenameTemplate: "{{pub_date}}_{{title}}_{{id}}",
	}

	out, err := Build(context.Background(), &feed, &cfg, "http://localhost/")
	require.NoError(t, err)
	require.Len(t, out.Items, 1)
	require.NotNil(t, out.Items[0].Enclosure)
	assert.Equal(t, "video123", out.Items[0].GUID)
	assert.Equal(t, "http://localhost/test/2025-12-31_A_title_with_chars_video123.mp4", out.Items[0].Enclosure.URL)
}

func TestEpisodeNameTemplate(t *testing.T) {
	cfg := &Config{
		ID:               "test",
		Format:           model.FormatVideo,
		FilenameTemplate: "{{pub_date}}_{{title}}_{{id}}",
	}

	episode := &model.Episode{
		ID:      "abc123",
		Title:   "My / Video: Title?",
		PubDate: time.Date(2026, 2, 8, 10, 0, 0, 0, time.UTC),
	}

	assert.Equal(t, "2026-02-08_My_Video_Title_abc123.mp4", EpisodeName(cfg, episode))
}

func TestValidateFilenameTemplate(t *testing.T) {
	assert.NoError(t, ValidateFilenameTemplate(""))
	assert.NoError(t, ValidateFilenameTemplate("{{pub_date}}_{{title}}_{{id}}"))
	assert.Error(t, ValidateFilenameTemplate("{{unknown}}_{{id}}"))
	assert.Error(t, ValidateFilenameTemplate("{{ID}}_{{id}}"))
	assert.Error(t, ValidateFilenameTemplate("{{pub-date}}_{{id}}"))
}

func TestValidateCustomExtension(t *testing.T) {
	assert.NoError(t, ValidateCustomExtension(".M4A"))
	assert.Error(t, ValidateCustomExtension(""))
	assert.Error(t, ValidateCustomExtension("../mp3"))
}

func TestEpisodeNameWithCustomExtensionNormalization(t *testing.T) {
	cfg := &Config{
		ID:     "test",
		Format: model.FormatCustom,
		CustomFormat: CustomFormat{
			Extension: ".M4A",
		},
	}
	episode := &model.Episode{ID: "abc123", Title: "Title"}
	assert.Equal(t, "abc123.m4a", EpisodeName(cfg, episode))

	cfg.CustomFormat.Extension = "../bad"
	assert.Equal(t, "abc123.mp4", EpisodeName(cfg, episode))
}

func TestBuildXMLEpisodeCoverArt(t *testing.T) {
	for _, cover := range []string{"", "auto", "https://example.com/cover.jpg?size=1400&v=2"} {
		t.Run(cover, func(t *testing.T) {
			f := model.Feed{
				CoverArt: "https://example.com/source-show.jpg",
				Episodes: []*model.Episode{
					{ID: "one", Title: "One", Status: model.EpisodeDownloaded, Thumbnail: "https://example.com/one.jpg", VideoURL: "https://youtube.com/watch?v=one"},
					{ID: "two", Title: "Two", Status: model.EpisodeDownloaded, Thumbnail: "https://example.com/two.jpg"},
					{ID: "pending", Status: model.EpisodeNew},
				},
			}
			cfg := Config{ID: "test", Custom: Custom{CoverArt: "https://example.com/show.jpg", EpisodeCoverArt: cover}}
			out, err := Build(context.Background(), &f, &cfg, "https://podcast.example.com")
			require.NoError(t, err)
			var rss struct {
				Channel struct {
					Image struct {
						Href string `xml:"href,attr"`
					} `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
					Items []struct {
						GUID  string `xml:"guid"`
						Link  string `xml:"link"`
						Image struct {
							Href string `xml:"href,attr"`
						} `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
						Enclosure struct {
							URL  string `xml:"url,attr"`
							Type string `xml:"type,attr"`
						} `xml:"enclosure"`
					} `xml:"item"`
				} `xml:"channel"`
			}
			require.NoError(t, xml.Unmarshal([]byte(out.String()), &rss))
			assert.Equal(t, cfg.Custom.CoverArt, rss.Channel.Image.Href)
			require.Len(t, rss.Channel.Items, 2)
			for _, item := range rss.Channel.Items {
				want := cover
				if want == "" || want == "auto" {
					want = "https://example.com/" + item.GUID + ".jpg"
				}
				assert.Equal(t, want, item.Image.Href)
				assert.Equal(t, "https://podcast.example.com/test/"+item.GUID+".mp4", item.Enclosure.URL)
				assert.Equal(t, "video/mp4", item.Enclosure.Type)
				if item.GUID == "one" {
					assert.Equal(t, "https://youtube.com/watch?v=one", item.Link)
				}
			}
			assert.Equal(t, "https://example.com/one.jpg", f.Episodes[0].Thumbnail)
		})
	}
}

func TestIDFilenamesPreserveEdgeCharacters(t *testing.T) {
	for _, template := range []string{"", "{{id}}", " {{ id }} "} {
		cfg := &Config{Format: model.FormatVideo, FilenameTemplate: template}
		for _, id := range []string{"-synthetic-", "synthetic--", "--synthetic", "_synthetic_", "synthetic"} {
			ep := &model.Episode{ID: id}
			assert.Equal(t, id+".mp4", EpisodeName(cfg, ep))
			assert.Equal(t, id+".mp4", LegacyEpisodeName(cfg, ep))
			names := EpisodeNameCandidates(cfg, ep)
			assert.Equal(t, id+".mp4", names[0])
			ep.MediaFilename = "../../outside.mp4"
			assert.Equal(t, id+".mp4", EpisodeName(cfg, ep))
			ep.MediaFilename = names[len(names)-1]
			assert.Equal(t, names[len(names)-1], EpisodeName(cfg, ep))
		}
	}
	cfg := &Config{Format: model.FormatVideo, FilenameTemplate: "{{title}}-{{id}}"}
	ep := &model.Episode{ID: "-synthetic-", Title: "../Title"}
	assert.Equal(t, "Title--synthetic.mp4", EpisodeName(cfg, ep))
	assert.Equal(t, []string{"Title--synthetic.mp4", "-synthetic-.mp4", "synthetic.mp4"}, EpisodeNameCandidates(cfg, ep))
	for _, id := range []string{"../../escape", " /unsafe ", ".", "..", "?query#fragment"} {
		cfg.FilenameTemplate = ""
		ep.ID = id
		for _, name := range EpisodeNameCandidates(cfg, ep) {
			assert.NotContains(t, name, "/")
			assert.NotContains(t, name, "\\")
			assert.NotContains(t, name, "?")
			assert.NotContains(t, name, "#")
		}
	}
}
