//go:build eval

// Package eval is the golden-eval harness that gates model/prompt changes for the
// vision pipeline (CON-281 NFR). It runs against the REAL Gemini model, so it is
// behind the `eval` build tag and skips when GEMINI_API_KEY is unset — advisory
// on forks, enforced on the canonical repo (see .github/workflows/ci.yml).
//
// It is deliberately a thin harness stub: it wires the real imgengine + vision
// pipeline to the fixtures under fixtures/, and asserts classification /
// extraction / alt-text expectations declared in fixtures/manifest.json. Add a
// fixture (and a manifest entry) for every prompt regression you fix.
package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/image-service/internal/imgengine"
	"github.com/ogen-app/image-service/internal/vision"
)

// fixtureCase mirrors one entry in fixtures/manifest.json. See eval/README.md for
// the field semantics.
type fixtureCase struct {
	File                string   `json:"file"`
	ExpectedShape       string   `json:"expected_shape"`
	MinBlocks           int      `json:"min_blocks"`
	MustContainText     []string `json:"must_contain_text"`
	ExpectLowConfidence bool     `json:"expect_low_confidence"`
	AltNotEmpty         bool     `json:"alt_not_empty"`
	AltTextMaxChars     int      `json:"alt_text_max_chars"`
}

const (
	// The eval uses the same defaults as production; a model bump should be
	// evaluated by changing these and re-running the gate.
	classifyModel = "gemini-2.5-flash"
	extractModel  = "gemini-2.5-pro"
	escalateModel = "gemini-2.5-pro"
)

// TestGoldenEval runs the full pipeline over every fixture and asserts the
// declared expectations. It skips when no API key is configured or the manifest
// is absent/empty (so a fresh checkout without curated fixtures is green).
func TestGoldenEval(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set; skipping golden eval (advisory)")
	}

	cases := loadManifest(t)
	if len(cases) == 0 {
		t.Skip("no fixtures in fixtures/manifest.json; nothing to evaluate")
	}

	// A local engine (libvips) reads the fixture bytes and produces the normalized
	// derivative the vision pass consumes — the same path production uses, minus
	// the presigned HTTP transfer (fixtures are read from disk).
	engine, err := imgengine.New(imgengine.Config{})
	if err != nil {
		t.Fatalf("imgengine.New: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	client, err := vision.New(context.Background(), apiKey)
	if err != nil {
		t.Fatalf("vision.New: %v", err)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.File, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			imgBytes, mime := loadFixture(t, tc.File)

			// Serve the fixture over a throwaway HTTP server so the REAL engine path
			// (fetch → libvips validate/bomb-guard/normalize → derivative) is
			// exercised — exactly as production runs, minus the presigned S3
			// transfer. The vision pass then consumes the engine's derivative, not
			// the raw fixture, so this gate covers normalization as it claims to.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", mime)
					_, _ = w.Write(imgBytes)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body) // PUT of the normalized derivative
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			eng, err := engine.Extract(ctx, srv.URL+"/src", srv.URL+"/dst", false, false)
			if err != nil {
				t.Fatalf("engine.Extract: %v", err)
			}

			res, err := client.Extract(ctx, vision.Image{
				Bytes:           eng.Bytes,
				MIME:            eng.DerivedMIME,
				ClassifyModel:   classifyModel,
				ExtractModel:    extractModel,
				EscalateModel:   escalateModel,
				AltTextMaxChars: tc.AltTextMaxChars,
			})
			if err != nil {
				t.Fatalf("vision.Extract: %v", err)
			}

			// Classification.
			if got := res.Shape.String(); tc.ExpectedShape != "" && got != tc.ExpectedShape {
				t.Errorf("shape = %q, want %q (classify_confidence=%.2f)", got, tc.ExpectedShape, res.ClassifyConfidence)
			}

			// Extraction: block count + required substrings.
			if len(res.Blocks) < tc.MinBlocks {
				t.Errorf("blocks = %d, want >= %d", len(res.Blocks), tc.MinBlocks)
			}
			allText := strings.ToLower(concatBlockText(res.Blocks))
			for _, want := range tc.MustContainText {
				if !strings.Contains(allText, strings.ToLower(want)) {
					t.Errorf("extracted text missing %q", want)
				}
			}
			if tc.ExpectLowConfidence && !anyLowConfidence(res.Blocks) {
				t.Errorf("expected a low_confidence block (tabular marker), got none")
			}

			// Alt text.
			if tc.AltNotEmpty && strings.TrimSpace(res.AltText) == "" {
				t.Errorf("alt text is empty")
			}
			if tc.AltTextMaxChars > 0 && len([]rune(res.AltText)) > tc.AltTextMaxChars {
				t.Errorf("alt text length %d exceeds cap %d", len([]rune(res.AltText)), tc.AltTextMaxChars)
			}
		})
	}
}

func loadManifest(t *testing.T) []fixtureCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read manifest: %v", err)
	}
	var cases []fixtureCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return cases
}

func loadFixture(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return b, mimeFromName(name)
}

func mimeFromName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".bmp":
		return "image/bmp"
	case ".heic", ".heif":
		return "image/heic"
	case ".avif":
		return "image/avif"
	default:
		return "image/png"
	}
}

func concatBlockText(blocks []vision.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Text)
		b.WriteByte('\n')
		for _, c := range blk.Cells {
			b.WriteString(c.Text)
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func anyLowConfidence(blocks []vision.Block) bool {
	for _, b := range blocks {
		if b.LowConfidence {
			return true
		}
	}
	return false
}
