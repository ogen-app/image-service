package imgengine

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// previewServer serves src on GET /src and records the body and Content-Type
// PUT to /dest.
type previewServer struct {
	*httptest.Server
	mu          sync.Mutex
	put         []byte
	contentType string
}

func newPreviewServer(t *testing.T, src []byte) *previewServer {
	t.Helper()
	ps := &previewServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/src":
			_, _ = w.Write(src)
		case r.Method == http.MethodPut && r.URL.Path == "/dest":
			b, _ := io.ReadAll(r.Body)
			ps.mu.Lock()
			ps.put, ps.contentType = b, r.Header.Get("Content-Type")
			ps.mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ps.Close)
	return ps
}

// written returns what was PUT to /dest.
func (ps *previewServer) written() ([]byte, string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.put, ps.contentType
}

// encodeJPEG returns an opaque w×h JPEG.
func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// encodeAlphaPNG returns a w×h PNG with a translucent pixel.
func encodeAlphaPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{R: 10, A: 128})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newTestEngine starts an engine with default limits.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestRenderPreview_DownscalesLongEdge checks an oversized opaque image is
// scaled to the cap and PUT as JPEG.
func TestRenderPreview_DownscalesLongEdge(t *testing.T) {
	e := newTestEngine(t)
	srv := newPreviewServer(t, encodeJPEG(t, 6000, 3000))

	res, err := e.RenderPreview(context.Background(), srv.URL+"/src", srv.URL+"/dest", 4096)
	if err != nil {
		t.Fatalf("RenderPreview: %v", err)
	}
	if res.MIME != "image/jpeg" || res.Width != 4096 || res.Height != 2048 {
		t.Fatalf("got %s %dx%d, want image/jpeg 4096x2048", res.MIME, res.Width, res.Height)
	}
	put, ct := srv.written()
	if ct != "image/jpeg" || int64(len(put)) != res.SizeBytes {
		t.Fatalf("PUT %q %d bytes, want image/jpeg %d bytes", ct, len(put), res.SizeBytes)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(put))
	if err != nil || cfg.Width != 4096 || cfg.Height != 2048 {
		t.Fatalf("written preview %dx%d (%v), want 4096x2048", cfg.Width, cfg.Height, err)
	}
}

// TestRenderPreview_NeverUpscalesAndKeepsAlpha checks a small image keeps its
// size and an alpha image stays PNG.
func TestRenderPreview_NeverUpscalesAndKeepsAlpha(t *testing.T) {
	e := newTestEngine(t)
	srv := newPreviewServer(t, encodeAlphaPNG(t, 300, 200))

	res, err := e.RenderPreview(context.Background(), srv.URL+"/src", srv.URL+"/dest", 4096)
	if err != nil {
		t.Fatalf("RenderPreview: %v", err)
	}
	if res.MIME != "image/png" || res.Width != 300 || res.Height != 200 {
		t.Fatalf("got %s %dx%d, want image/png 300x200", res.MIME, res.Width, res.Height)
	}
	if _, ct := srv.written(); ct != "image/png" {
		t.Fatalf("PUT Content-Type %q, want image/png", ct)
	}
}
