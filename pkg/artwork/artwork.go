// Package artwork creates bounded, opaque square artwork from YouTube thumbnails.
package artwork

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // Register PNG decoding for thumbnails with transparency.
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	Size         = 1400
	maxBytes     = 8 << 20
	maxPixels    = 16_000_000
	maxDimension = 8192
)

type Generator struct{ client *http.Client }

// New uses the supplied transport when present, but always bounds time and redirects.
func New(client *http.Client) *Generator {
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.Timeout = 10 * time.Second
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many thumbnail redirects")
		}
		return validateURL(req.URL)
	}
	return &Generator{client: &c}
}

func validateURL(u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") ||
		(host != "img.youtube.com" && host != "ytimg.com" && !strings.HasSuffix(host, ".ytimg.com")) {
		return fmt.Errorf("thumbnail URL must use HTTPS on a YouTube image host")
	}
	return nil
}

// Generate tries metadata candidates in resolution order. No URL is synthesized.
func (g *Generator) Generate(ctx context.Context, candidates []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var lastErr error
	for _, candidate := range candidates {
		data, err := g.fetch(ctx, candidate)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("no usable thumbnail: %v", lastErr)
}

func (g *Generator) fetch(ctx context.Context, source string) ([]byte, error) {
	u, err := url.Parse(source)
	if err != nil {
		return nil, err
	}
	if err := validateURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thumbnail returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("thumbnail exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("thumbnail exceeds byte limit")
	}
	return square(data)
}

func square(data []byte) ([]byte, error) {
	return renderSquare(data, 0.15)
}

// renderSquare exposes the background-only shade to package tests and deterministic previews.
func renderSquare(data []byte, backgroundDarkening float64) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if format != "jpeg" && format != "png" {
		return nil, fmt.Errorf("unsupported thumbnail format %q", format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxDimension || cfg.Height > maxDimension ||
		int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("thumbnail exceeds pixel limit")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	// Blur a bounded working image, then enlarge it to the full canvas. The Gaussian
	// has an effective sigma of 40 canvas pixels. Clamp edges instead of sampling
	// transparent/black pixels outside the image, avoiding dark blur borders.
	background := image.NewRGBA(image.Rect(0, 0, 280, 280))
	draw.Draw(background, background.Bounds(), image.Black, image.Point{}, draw.Src)
	draw.Draw(background, background.Bounds(), resize(src, 280, 280), image.Point{}, draw.Over)
	canvas := resize(gaussian(background, 8), Size, Size)
	// Composite a 15% black overlay on the background before adding the clear foreground.
	for i := 0; i < len(canvas.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			canvas.Pix[i+c] = uint8(math.Round(float64(canvas.Pix[i+c]) * (1 - backgroundDarkening)))
		}
	}
	w, h := Size, Size
	if cfg.Width >= cfg.Height {
		h = max(1, int(math.Round(float64(Size)*float64(cfg.Height)/float64(cfg.Width))))
	} else {
		w = max(1, int(math.Round(float64(Size)*float64(cfg.Width)/float64(cfg.Height))))
	}
	foreground := resize(src, w, h)
	rect := image.Rect((Size-w)/2, (Size-h)/2, (Size-w)/2+w, (Size-h)/2+h)
	draw.Draw(canvas, rect, foreground, image.Point{}, draw.Over)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// resize bilinearly samples premultiplied RGBA, preserving transparency for compositing.
func resize(src image.Image, w, h int) *image.RGBA {
	result := image.NewRGBA(image.Rect(0, 0, w, h))
	bounds := src.Bounds()
	for y := 0; y < h; y++ {
		sy := max(0, (float64(y)+0.5)*float64(bounds.Dy())/float64(h)-0.5)
		y0 := int(sy)
		y1 := min(y0+1, bounds.Dy()-1)
		fy := sy - float64(y0)
		for x := 0; x < w; x++ {
			sx := max(0, (float64(x)+0.5)*float64(bounds.Dx())/float64(w)-0.5)
			x0 := int(sx)
			x1 := min(x0+1, bounds.Dx()-1)
			fx := sx - float64(x0)
			r00, g00, b00, a00 := src.At(bounds.Min.X+x0, bounds.Min.Y+y0).RGBA()
			r10, g10, b10, a10 := src.At(bounds.Min.X+x1, bounds.Min.Y+y0).RGBA()
			r01, g01, b01, a01 := src.At(bounds.Min.X+x0, bounds.Min.Y+y1).RGBA()
			r11, g11, b11, a11 := src.At(bounds.Min.X+x1, bounds.Min.Y+y1).RGBA()
			blend := func(a, b, c, d uint32) uint8 {
				return uint8(math.Round(((float64(a)*(1-fx)+float64(b)*fx)*(1-fy) + (float64(c)*(1-fx)+float64(d)*fx)*fy) / 257))
			}
			result.SetRGBA(x, y, color.RGBA{R: blend(r00, r10, r01, r11), G: blend(g00, g10, g01, g11), B: blend(b00, b10, b01, b11), A: blend(a00, a10, a01, a11)})
		}
	}
	return result
}

// gaussian is a normalized, separable Gaussian convolution with clamped edges.
// The input is opaque; both passes keep the output opaque.
func gaussian(src *image.RGBA, sigma float64) *image.RGBA {
	radius := int(math.Ceil(3 * sigma))
	weights := make([]float64, 2*radius+1)
	total := 0.0
	for i := range weights {
		x := float64(i - radius)
		weights[i] = math.Exp(-x * x / (2 * sigma * sigma))
		total += weights[i]
	}
	for i := range weights {
		weights[i] /= total
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	for _, horizontal := range []bool{true, false} {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var r, g, b float64
				for k, weight := range weights {
					sx, sy := x, y
					if horizontal {
						sx = max(0, min(w-1, x+k-radius))
					} else {
						sy = max(0, min(h-1, y+k-radius))
					}
					i := sy*src.Stride + sx*4
					r += float64(src.Pix[i]) * weight
					g += float64(src.Pix[i+1]) * weight
					b += float64(src.Pix[i+2]) * weight
				}
				dst.SetRGBA(x, y, color.RGBA{R: uint8(math.Round(r)), G: uint8(math.Round(g)), B: uint8(math.Round(b)), A: 255})
			}
		}
		src = dst
	}
	return src
}
