package artwork

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

func TestSquarePreservesImage(t *testing.T) {
	for _, shape := range []image.Point{{X: 400, Y: 200}, {X: 200, Y: 400}, {X: 200, Y: 200}} {
		t.Run(shape.String(), func(t *testing.T) {
			source := image.NewNRGBA(image.Rect(0, 0, shape.X, shape.Y))
			// Four colored quadrants and their outer edges must remain present after fitting.
			colors := []color.NRGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}}
			for y := 0; y < shape.Y; y++ {
				for x := 0; x < shape.X; x++ {
					source.SetNRGBA(x, y, colors[(y/(shape.Y/2))*2+x/(shape.X/2)])
				}
			}
			data, err := square(pngBytes(t, source))
			require.NoError(t, err)
			result, err := jpeg.Decode(bytes.NewReader(data))
			require.NoError(t, err)
			assert.Equal(t, image.Rect(0, 0, 1400, 1400), result.Bounds())
			w, h := Size, Size
			if shape.X > shape.Y {
				h = 700
			}
			if shape.Y > shape.X {
				w = 700
			}
			left, top := (Size-w)/2, (Size-h)/2
			for i, p := range []image.Point{{X: left + 10, Y: top + 10}, {X: left + w - 11, Y: top + 10}, {X: left + 10, Y: top + h - 11}, {X: left + w - 11, Y: top + h - 11}} {
				r, g, b, a := result.At(p.X, p.Y).RGBA()
				want := colors[i]
				assert.InDelta(t, int(want.R)*257, int(r), 1000)
				assert.InDelta(t, int(want.G)*257, int(g), 1000)
				assert.InDelta(t, int(want.B)*257, int(b), 1000)
				assert.Equal(t, uint32(65535), a)
			}
			if top > 0 {
				r, g, b, a := result.At(Size/2, 10).RGBA()
				assert.Greater(t, r+g+b, uint32(1000))
				assert.Equal(t, uint32(65535), a)
			}
			if left > 0 {
				r, g, b, a := result.At(10, Size/2).RGBA()
				assert.Greater(t, r+g+b, uint32(1000))
				assert.Equal(t, uint32(65535), a)
			}
		})
	}
}

func TestTransparentInputBecomesOpaqueJPEG(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 255, A: 128})
		}
	}
	data, err := square(pngBytes(t, src))
	require.NoError(t, err)
	img, format, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	r, g, b, a := img.At(700, 700).RGBA()
	assert.InDelta(t, 173*257, r, 1000)
	assert.Less(t, g, uint32(1000))
	assert.Less(t, b, uint32(1000))
	assert.Equal(t, uint32(65535), a)
}

func TestGenerateFallbackAndLimits(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 4, 2))
	valid := pngBytes(t, source)
	// A valid PNG header declaring excessive pixels must fail before any image allocation.
	oversized := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(oversized[16:20], 8192)
	binary.BigEndian.PutUint32(oversized[20:24], 8192)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		length int64
	}{
		{name: "missing", status: 404}, {name: "invalid", status: 200, body: []byte("not an image")},
		{name: "pixels", status: 200, body: oversized}, {name: "header size", status: 200, length: maxBytes + 1},
		{name: "stream size", status: 200, body: bytes.Repeat([]byte{'x'}, maxBytes+1), length: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			generator := New(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.Path)
				status, body, length := tc.status, tc.body, tc.length
				if r.URL.Path == "/good" {
					status, body, length = 200, valid, int64(len(valid))
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: length, Header: make(http.Header), Request: r}, nil
			})})
			_, err := generator.Generate(context.Background(), []string{"https://i.ytimg.com/bad"})
			require.Error(t, err)
			calls = nil
			data, err := generator.Generate(context.Background(), []string{"https://i.ytimg.com/bad", "https://i.ytimg.com/good"})
			require.NoError(t, err)
			require.NotEmpty(t, data)
			assert.Equal(t, []string{"/bad", "/good"}, calls)
		})
	}
}

func TestURLAndRedirectSafety(t *testing.T) {
	for _, raw := range []string{"http://i.ytimg.com/a", "https://localhost/a", "https://i.ytimg.com.evil.test/a", "https://user@i.ytimg.com/a", "https://i.ytimg.com:8443/a", "file:///etc/passwd"} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Error(t, validateURL(u))
	}
	var calls int
	generator := New(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})})
	_, err := generator.Generate(context.Background(), []string{"https://i.ytimg.com/redirect"})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 10*time.Second, generator.client.Timeout)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	generator = New(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })})
	_, err = generator.Generate(ctx, []string{"https://i.ytimg.com/canceled"})
	assert.Error(t, err)
}

func TestGaussianClampAndSmoothing(t *testing.T) {
	solid := image.NewRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			solid.SetRGBA(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	blurred := gaussian(solid, 8)
	for _, p := range []image.Point{{X: 0, Y: 0}, {X: 19, Y: 0}, {X: 0, Y: 19}, {X: 19, Y: 19}, {X: 10, Y: 10}} {
		assert.Equal(t, color.RGBA{R: 200, G: 100, B: 50, A: 255}, blurred.RGBAAt(p.X, p.Y))
	}
	impulse := image.NewRGBA(image.Rect(0, 0, 9, 9))
	impulse.SetRGBA(4, 4, color.RGBA{R: 255, A: 255})
	blurred = gaussian(impulse, 1)
	assert.Greater(t, blurred.RGBAAt(4, 4).R, blurred.RGBAAt(5, 4).R)
	assert.Greater(t, blurred.RGBAAt(5, 4).R, blurred.RGBAAt(6, 4).R)
	assert.Greater(t, blurred.RGBAAt(5, 4).R, uint8(0))
}

func TestSquareBlurredBackgroundHasNoBlackBorder(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for y := 0; y < 9; y++ {
		for x := 0; x < 16; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	data, err := square(pngBytes(t, src))
	require.NoError(t, err)
	img, err := jpeg.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	for _, p := range []image.Point{{X: 0, Y: 0}, {X: 1399, Y: 0}, {X: 0, Y: 1399}, {X: 1399, Y: 1399}} {
		r, g, b, a := img.At(p.X, p.Y).RGBA()
		assert.InDelta(t, 140*257, r, 1000)
		assert.InDelta(t, 70*257, g, 1000)
		assert.InDelta(t, 35*257, b, 1000)
		assert.Equal(t, uint32(65535), a)
	}
}

func TestBackgroundDarkeningLeavesForegroundUnchanged(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for y := 0; y < 9; y++ {
		for x := 0; x < 16; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	data := pngBytes(t, src)
	plain, err := renderSquare(data, 0)
	require.NoError(t, err)
	dark, err := square(data)
	require.NoError(t, err)
	a, err := jpeg.Decode(bytes.NewReader(plain))
	require.NoError(t, err)
	b, err := jpeg.Decode(bytes.NewReader(dark))
	require.NoError(t, err)
	for _, p := range []image.Point{{X: 100, Y: 100}, {X: 700, Y: 1300}} {
		r0, g0, b0, _ := a.At(p.X, p.Y).RGBA()
		r1, g1, b1, alpha := b.At(p.X, p.Y).RGBA()
		assert.InDelta(t, float64(r0)*0.7, r1, 1000)
		assert.InDelta(t, float64(g0)*0.7, g1, 1000)
		assert.InDelta(t, float64(b0)*0.7, b1, 1000)
		assert.Equal(t, uint32(65535), alpha)
	}
	for _, p := range []image.Point{{X: 100, Y: 400}, {X: 700, Y: 700}, {X: 1300, Y: 1000}} {
		r0, g0, b0, _ := a.At(p.X, p.Y).RGBA()
		r1, g1, b1, alpha := b.At(p.X, p.Y).RGBA()
		assert.InDelta(t, r0, r1, 300)
		assert.InDelta(t, g0, g1, 300)
		assert.InDelta(t, b0, b1, 300)
		assert.Equal(t, uint32(65535), alpha)
	}
}
