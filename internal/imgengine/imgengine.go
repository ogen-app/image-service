// Package imgengine is image-service's libvips-backed image compute (CON-281). It
// is the CGO counterpart of pdf-service's pdfengine: the native library
// (libvips, via govips) is exactly why this is a separate deployable rather than
// a package in the CGO_ENABLED=0 Ogen API — decoding HEIC/AVIF/TIFF and doing a
// bomb-safe normalize needs a real image library, kept out of the API's static
// binary (decision — mirror pdf-service).
//
// It exposes two entry points, one per ingress path (decision D6 — image-service
// is the SOLE image ingress):
//
//   - Extract           — the full content-bank path. Re-encodes to PNG or JPEG,
//     PRESERVES resolution for text-bearing images (so the
//     downstream Gemini extract pass can read small text) and
//     downscales only non-text images past a cap. Strips
//     EXIF/geo. Returns the derivative's metadata.
//   - PrepareAttachment — the light post-attachment path. Strips EXIF/geo with
//     PIXELS PRESERVED — no resize, and no lossy re-encode
//     where the format lets metadata be dropped in place; a
//     format that can't be stripped without re-encoding is
//     re-encoded LOSSLESSLY to the same format. This is the
//     net-new privacy fix: geolocation no longer leaks to
//     Zernio on publish.
//
// Safety invariants shared by both paths:
//
//   - Header-first bomb guard. Dimensions are read from the header and rejected
//     against MaxPixels BEFORE any pixel buffer is decoded, so a "42000x42000
//     PNG" that would allocate tens of GB never gets decoded. A second guard
//     bounds the fetched ENCODED byte size (MaxUploadBytes) — a small-dimension
//     image can still be a huge payload.
//   - SVG / vector is rejected terminally (ErrVector): libvips would rasterize it
//     at an attacker-chosen scale, and a vector has no honest pixel dimensions to
//     bomb-guard. ogen surfaces this as an InvalidArgument.
//   - The SHA-256 checksum is always computed over the ORIGINAL fetched bytes
//     (ogen dedupes on it), never over the derivative.
//   - Native memory is bounded by the worker semaphore + MaxPixels; the whole
//     original is held once in memory (images are small relative to audio/video),
//     unlike audio-service which streams.
package imgengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	// Register the stdlib header decoders for image.DecodeConfig. These parse
	// ONLY the header (no pixels), which is exactly what the pre-decode bomb guard
	// needs for JPEG/PNG/GIF. WebP/BMP/TIFF/HEIC/AVIF are parsed by hand below.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
)

// ─── Domain error sentinels (mapped to gRPC codes at the server boundary) ─────
//
// Terminal content verdicts (the client must NOT retry) each get a distinct
// sentinel so the server can classify them while they all land on
// InvalidArgument. Transport/decode faults that are NOT content verdicts stay
// plain errors → Internal (transient) so a valid upload is never wrongly
// rejected on a blip.

// ErrUnsupported marks input in a raster format libvips loaded but that the
// service does not accept (outside the JPEG/PNG/WebP/HEIC/AVIF/GIF/TIFF/BMP
// allow-list). Terminal.
var ErrUnsupported = errors.New("imgengine: unsupported image format")

// ErrVector marks SVG / any vector input. Terminal: a vector has no honest pixel
// dimensions to bomb-guard and libvips would rasterize it at an attacker-chosen
// scale, so it is rejected outright rather than normalized (decision — SVG
// reject).
var ErrVector = errors.New("imgengine: vector images (svg) are not supported")

// ErrCorrupt marks input libvips could not decode as an image (truncated,
// malformed, or not an image at all). Terminal.
var ErrCorrupt = errors.New("imgengine: corrupt or undecodable image")

// ErrOversize marks input that exceeds the pixel-area (MaxPixels) or encoded-byte
// (MaxUploadBytes) bomb guard. Terminal — the client must shrink it, not retry.
var ErrOversize = errors.New("imgengine: image exceeds size limits")

// ErrFetch marks a failure fetching the source or PUTting the derivative over
// HTTP. NOT a content verdict — transient, so the server maps it to Internal and
// the caller can retry the whole step.
var ErrFetch = errors.New("imgengine: presigned transfer failed")

// ─── Config / Engine ─────────────────────────────────────────────────────────

// Config tunes the engine.
type Config struct {
	// MaxPixels is the header-first bomb guard: width*height above this is
	// rejected before decode. See config.MaxPixels.
	MaxPixels int64
	// MaxUploadBytes bounds the fetched encoded byte size. See config.MaxUploadBytes.
	MaxUploadBytes int64
	// HTTPTimeout bounds a single presigned GET/PUT round-trip.
	HTTPTimeout time.Duration
	// Workers bounds concurrent decode/encode operations (peak native RSS).
	Workers int
	// ScavengeOnIdle returns freed memory to the OS once the pool drains.
	ScavengeOnIdle bool
	// ExtractDownscaleMaxDim caps the longest edge of a NON-TEXT Extract
	// derivative (text-bearing shapes keep full resolution). 0 => default.
	ExtractDownscaleMaxDim int
	// JPEGQuality is the quality used when an Extract derivative is re-encoded as
	// JPEG (opaque, non-text images). 0 => default.
	JPEGQuality int
}

// Engine runs libvips operations, bounded by a worker semaphore. It owns a
// single http.Client for presigned transfers.
type Engine struct {
	maxPixels       int64
	maxUploadBytes  int64
	downscaleMaxDim int
	jpegQuality     int
	httpClient      *http.Client
	sem             chan struct{}

	// scavengeOnIdle returns freed memory to the OS once the pool drains after a
	// burst; active tracks in-flight work so the drain-to-idle edge can be
	// detected, and scavenging collapses overlapping triggers into one. scavengeFn
	// is the actual reclaim (debug.FreeOSMemory), injectable for tests.
	scavengeOnIdle bool
	scavengeFn     func()
	active         atomic.Int64
	scavenging     atomic.Bool
}

// defaults for the tunables that have a natural value.
const (
	defaultMaxPixels       = 100_000_000 // 100 MP
	defaultMaxUploadBytes  = 64 << 20    // 64 MiB
	defaultDownscaleMaxDim = 4096        // longest edge for non-text derivatives
	defaultJPEGQuality     = 85
	defaultWorkers         = 4
	defaultHTTPTimeout     = 60 * time.Second
)

// New initialises libvips and the worker pool. libvips MUST be started once per
// process before any image op (vips.Startup); the caller Close()s to shut it
// down. It fails fast if libvips isn't linkable so a misconfigured deploy
// surfaces at boot, not on the first request — the same fail-fast contract as
// pdfengine.New / audioengine.New.
func New(cfg Config) (*Engine, error) {
	// Route libvips' own diagnostics through slog and keep its internal operation
	// cache small — the service is stateless and each op is one-shot, so a warm
	// cache only holds memory. ReportLeaks helps catch a native ref leak in tests.
	vips.LoggingSettings(vipsSlogBridge, vips.LogLevelWarning)
	vips.Startup(&vips.Config{
		ConcurrencyLevel: 1, // per-op threads; the worker sem caps overall concurrency
		MaxCacheMem:      0, // no operation-result cache (stateless, one-shot ops)
		MaxCacheSize:     0,
		ReportLeaks:      false, // flip on in tests to assert no native ref leaks
	})

	workers := cfg.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	return &Engine{
		maxPixels:       orInt64(cfg.MaxPixels, defaultMaxPixels),
		maxUploadBytes:  orInt64(cfg.MaxUploadBytes, defaultMaxUploadBytes),
		downscaleMaxDim: orInt(cfg.ExtractDownscaleMaxDim, defaultDownscaleMaxDim),
		jpegQuality:     orInt(cfg.JPEGQuality, defaultJPEGQuality),
		httpClient:      &http.Client{Timeout: orDuration(cfg.HTTPTimeout, defaultHTTPTimeout)},
		sem:             make(chan struct{}, workers),
		scavengeOnIdle:  cfg.ScavengeOnIdle,
		scavengeFn:      debug.FreeOSMemory,
	}, nil
}

// Close shuts libvips down, releasing its native thread pool and caches. Mirrors
// pdfengine/audioengine Close so cmd/ wiring is identical.
func (e *Engine) Close() error {
	vips.Shutdown()
	return nil
}

// ─── Result types ─────────────────────────────────────────────────────────────

// Meta is the metadata common to both paths, computed from the ORIGINAL bytes.
type Meta struct {
	MIME       string // sniffed authoritative MIME, e.g. "image/png"
	SizeBytes  int64  // original encoded byte size
	Width      int    // pixel width (of the original, orientation-applied)
	Height     int    // pixel height
	IsAnimated bool   // multi-frame (animated GIF/WebP)
	FrameCount int    // frames for animated input; 1 otherwise
	SHA256     string // hex SHA-256 of the ORIGINAL bytes
}

// ExtractResult is Extract's output: the source metadata plus the normalized
// derivative's shape and whether it kept full resolution (text-bearing) or was
// downscaled.
type ExtractResult struct {
	Meta          Meta
	DerivedMIME   string // "image/png" or "image/jpeg"
	DerivedWidth  int
	DerivedHeight int
	// Bytes is the normalized derivative, retained so the server can hand the SAME
	// pixels to the vision model without a second decode/fetch. It is also the
	// payload PUT to dest_put_url.
	Bytes []byte
}

// PrepareResult is PrepareAttachment's output: the source metadata plus the
// metadata-stripped (pixels-preserved) derivative bytes and the derivative's own
// MIME (which can differ from Meta.MIME — e.g. a HEIC original is re-emitted as
// lossless PNG). Callers PUT/serve/describe the derivative by DerivedMIME, not
// the original Meta.MIME.
type PrepareResult struct {
	Meta        Meta
	DerivedMIME string // MIME of Bytes (the stored derivative)
	Bytes       []byte // same pixels, EXIF/geo stripped; PUT to dest_put_url
}

// ─── Extract path ─────────────────────────────────────────────────────────────

// Extract fetches the image at sourceURL, validates + bomb-guards it, re-encodes
// a normalized derivative (full resolution for text-bearing shapes, downscaled
// for non-text), strips EXIF/geo, and PUTs the derivative to destPutURL. It
// returns the derivative bytes (for the vision pass) and metadata.
//
// textBearing is decided by the caller AFTER classify (a low-res classify pass
// runs on the derivative first, then — if it's prose/conversation/social/tabular
// — a full-res derivative is what the extract pass needs). To avoid a double
// re-encode, Extract always produces a FULL-RESOLUTION derivative here and lets
// the server decide downscaling for the STORED asset separately; passing
// downscaleNonText=true opts into downscaling when the image is known non-text.
//
// sourceNormalized=true skips the re-encode (the source is already a
// service-produced derivative) and just fetches + reads metadata.
func (e *Engine) Extract(ctx context.Context, sourceURL, destPutURL string, sourceNormalized, downscaleNonText bool) (*ExtractResult, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return nil, fmt.Errorf("%w: empty source url", ErrCorrupt)
	}

	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer e.releaseWorker()

	orig, err := e.fetch(ctx, sourceURL)
	if err != nil {
		return nil, err
	}

	meta, ref, err := e.loadAndGuard(orig)
	if err != nil {
		return nil, err
	}
	defer ref.Close()

	// sourceNormalized: the source URL already points at a service-produced
	// derivative (a re-extraction reading back normalized.png), so there is nothing
	// to re-encode and nothing new to PUT (the derivative already lives at dest).
	// Return the fetched bytes and their metadata for the vision pass. This is the
	// case the parameter documents; without this branch it was silently ignored and
	// the image was needlessly re-encoded every time.
	if sourceNormalized {
		return &ExtractResult{
			Meta:          meta,
			DerivedMIME:   meta.MIME,
			DerivedWidth:  ref.Width(),
			DerivedHeight: ref.Height(),
			Bytes:         orig,
		}, nil
	}

	// The stored derivative: strip metadata always; optionally downscale a
	// non-text image so the content bank doesn't hold a 100 MP photo. Text-bearing
	// images keep full resolution so the vision extract pass can read small text.
	if downscaleNonText {
		if err := downscaleToMaxDim(ref, e.downscaleMaxDim); err != nil {
			return nil, fmt.Errorf("%w: downscale: %v", ErrCorrupt, err)
		}
	}

	derived, derivedMIME, dw, dh, err := e.encodeDerivative(ref)
	if err != nil {
		return nil, err
	}

	// Skip the PUT when the source is already a derivative and unchanged — but the
	// common case still writes so ogen can rely on dest_put_url holding a
	// stripped/normalized copy.
	if strings.TrimSpace(destPutURL) != "" {
		if err := e.put(ctx, destPutURL, derived, derivedMIME); err != nil {
			return nil, err
		}
	}

	return &ExtractResult{
		Meta:          meta,
		DerivedMIME:   derivedMIME,
		DerivedWidth:  dw,
		DerivedHeight: dh,
		Bytes:         derived,
	}, nil
}

// ─── PrepareAttachment path ───────────────────────────────────────────────────

// PrepareAttachment fetches the image at sourceURL, validates + bomb-guards it,
// and — when stripMetadata is true — writes a derivative with EXIF/geo stripped
// but PIXELS PRESERVED (no resize, no lossy re-encode where avoidable) to
// destPutURL. It returns metadata computed from the ORIGINAL bytes.
//
// Pixels-preserved is the whole point of this path: a post attachment must
// publish byte-faithful to what the user uploaded (minus the privacy metadata),
// so we never downscale or transcode to a lossy format here. Where libvips can
// only re-emit by re-encoding, we re-encode LOSSLESSLY to the same format (PNG
// stays PNG lossless; JPEG is re-emitted at quality 100 with chroma subsampling
// off — the closest to lossless JPEG allows). Formats whose metadata can be
// dropped without touching pixels are handled that way.
func (e *Engine) PrepareAttachment(ctx context.Context, sourceURL, destPutURL string, stripMetadata bool) (*PrepareResult, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return nil, fmt.Errorf("%w: empty source url", ErrCorrupt)
	}

	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer e.releaseWorker()

	orig, err := e.fetch(ctx, sourceURL)
	if err != nil {
		return nil, err
	}

	meta, ref, err := e.loadAndGuard(orig)
	if err != nil {
		return nil, err
	}
	defer ref.Close()

	// The derivative starts as the original bytes (its MIME is the source MIME).
	// Stripping re-emits without metadata and may change the container (so the
	// derivative MIME can differ from the source), which is why we track it and
	// PUT/return by it — not by meta.MIME.
	derived, derivedMIME := orig, meta.MIME
	switch {
	case !stripMetadata:
		// Not stripping (rare; ogen always strips): pixels AND metadata untouched.
	case meta.IsAnimated:
		// Animated GIF/WebP: a single-frame re-encode would DROP every frame but the
		// first, silently turning an animation into a still. Preserve all frames by
		// passing the original bytes through unchanged. (Animated GIF carries no EXIF
		// to strip; stripping metadata from animated WebP while keeping frames needs
		// a multi-page re-export and is deferred — the priority is never destroying
		// the user's animation on a publishing artifact.)
	default:
		derived, derivedMIME, err = e.encodePixelsPreserved(ref, meta.MIME)
		if err != nil {
			return nil, err
		}
	}

	if strings.TrimSpace(destPutURL) != "" {
		if err := e.put(ctx, destPutURL, derived, derivedMIME); err != nil {
			return nil, err
		}
	}

	return &PrepareResult{Meta: meta, DerivedMIME: derivedMIME, Bytes: derived}, nil
}

// ─── Load, validate, bomb-guard ───────────────────────────────────────────────

// loadAndGuard sniffs the format, enforces the vector reject and both bomb
// guards, then decodes into a vips ref with orientation applied. It returns the
// original-bytes metadata alongside the ref. The caller owns ref.Close().
//
// Order matters for safety: the encoded-byte guard and the vector/allow-list
// checks run on the RAW bytes and header BEFORE the full pixel decode, so a bomb
// never reaches the decoder.
func (e *Engine) loadAndGuard(orig []byte) (Meta, *vips.ImageRef, error) {
	if int64(len(orig)) == 0 {
		return Meta{}, nil, fmt.Errorf("%w: empty body", ErrCorrupt)
	}
	if int64(len(orig)) > e.maxUploadBytes {
		return Meta{}, nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrOversize, len(orig), e.maxUploadBytes)
	}

	// HEADER-FIRST bomb guard. Read the encoded dimensions from the HEADER only —
	// no pixel decode — and reject a pixel area over MaxPixels before libvips ever
	// allocates a pixel buffer. This is the critical ordering: libvips'
	// new-from-buffer materializes pixels for random-access loaders, so a
	// "42000x42000 PNG" would allocate tens of GB if we decoded first. probeHeader
	// also does the SVG/vector reject (a vector has no honest pixel size to guard)
	// and the raster allow-list, so an unsupported or vector input never reaches
	// the decoder.
	fmtName, w, h, err := probeHeader(orig)
	if err != nil {
		return Meta{}, nil, err // already a wrapped Err* sentinel
	}
	if px := int64(w) * int64(h); px > e.maxPixels {
		return Meta{}, nil, fmt.Errorf("%w: %d pixels (%dx%d) exceeds %d",
			ErrOversize, px, w, h, e.maxPixels)
	}

	// Header is safe; now decode via libvips. AutoRotate at load applies the EXIF
	// orientation so the reported dimensions and any derivative are visually
	// upright; the orientation tag is then dropped with the rest of the metadata on
	// encode (StripMetadata). FailOnError=true so a truncated/corrupt payload that
	// slipped past the header probe still fails cleanly rather than yielding a
	// half-decoded image.
	params := vips.NewImportParams()
	params.FailOnError.Set(true)
	params.AutoRotate.Set(true)
	ref, err := vips.LoadImageFromBuffer(orig, params)
	if err != nil {
		return Meta{}, nil, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}

	// Belt-and-braces: re-check the DECODED dimensions against the cap. A
	// maliciously crafted header could under-report size to pass the pre-decode
	// guard; libvips' post-decode Width/Height are authoritative, so guard again
	// and close the ref if it lied.
	if px := int64(ref.Width()) * int64(ref.Height()); px > e.maxPixels {
		ref.Close()
		return Meta{}, nil, fmt.Errorf("%w: %d decoded pixels exceeds %d", ErrOversize, px, e.maxPixels)
	}

	frames := ref.Pages()
	if frames < 1 {
		frames = 1
	}
	sum := sha256.Sum256(orig)
	meta := Meta{
		MIME:       mimeForFormat(fmtName),
		SizeBytes:  int64(len(orig)),
		Width:      ref.Width(),
		Height:     ref.Height(),
		IsAnimated: frames > 1,
		FrameCount: frames,
		SHA256:     hex.EncodeToString(sum[:]),
	}
	return meta, ref, nil
}

// ─── Encoding ─────────────────────────────────────────────────────────────────

// encodeDerivative produces the Extract path's normalized derivative: an opaque
// image goes to JPEG (smaller for photos), an image WITH an alpha channel goes to
// PNG (JPEG can't carry alpha). Metadata is stripped on export. Returns bytes,
// MIME, and the derivative dimensions.
func (e *Engine) encodeDerivative(ref *vips.ImageRef) ([]byte, string, int, int, error) {
	if ref.HasAlpha() {
		params := vips.NewPngExportParams()
		params.StripMetadata = true
		b, _, err := ref.ExportPng(params)
		if err != nil {
			return nil, "", 0, 0, fmt.Errorf("%w: export png: %v", ErrCorrupt, err)
		}
		return b, "image/png", ref.Width(), ref.Height(), nil
	}
	params := vips.NewJpegExportParams()
	params.StripMetadata = true
	params.Quality = e.jpegQuality
	b, _, err := ref.ExportJpeg(params)
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("%w: export jpeg: %v", ErrCorrupt, err)
	}
	return b, "image/jpeg", ref.Width(), ref.Height(), nil
}

// encodePixelsPreserved produces the PrepareAttachment derivative: same pixels,
// metadata stripped. It re-emits in the SOURCE format so a JPEG stays a JPEG and
// a PNG stays a PNG — a post attachment must publish faithful to the upload. For
// JPEG we re-encode at quality 100 with subsampling disabled (the closest JPEG
// gets to lossless); PNG re-encodes losslessly by nature. WebP re-encodes
// lossless. Other allowed formats (HEIC/AVIF/TIFF/BMP/GIF) that don't round-trip
// cleanly fall back to a lossless PNG, since preserving the exact pixels matters
// more than preserving the container for a rarely-used input.
func (e *Engine) encodePixelsPreserved(ref *vips.ImageRef, sourceMIME string) ([]byte, string, error) {
	switch sourceMIME {
	case "image/jpeg":
		params := vips.NewJpegExportParams()
		params.StripMetadata = true
		params.Quality = 100
		params.SubsampleMode = vips.VipsForeignSubsampleOff // no chroma subsampling
		b, _, err := ref.ExportJpeg(params)
		if err != nil {
			return nil, "", fmt.Errorf("%w: strip jpeg: %v", ErrCorrupt, err)
		}
		return b, "image/jpeg", nil
	case "image/webp":
		params := vips.NewWebpExportParams()
		params.StripMetadata = true
		params.Lossless = true
		b, _, err := ref.ExportWebp(params)
		if err != nil {
			return nil, "", fmt.Errorf("%w: strip webp: %v", ErrCorrupt, err)
		}
		return b, "image/webp", nil
	default:
		// PNG for image/png and every other allowed still format: lossless,
		// alpha-safe, universally decodable. Pixels are preserved exactly; only the
		// container and the metadata change — so the derivative MIME becomes
		// image/png even when the source was e.g. HEIC/AVIF/TIFF/BMP.
		params := vips.NewPngExportParams()
		params.StripMetadata = true
		b, _, err := ref.ExportPng(params)
		if err != nil {
			return nil, "", fmt.Errorf("%w: strip png: %v", ErrCorrupt, err)
		}
		return b, "image/png", nil
	}
}

// downscaleToMaxDim shrinks ref so its longest edge is at most maxDim, preserving
// aspect ratio. A no-op when the image already fits or maxDim<=0. Used only for
// NON-TEXT Extract derivatives — downscaling text would cost the vision pass its
// ability to read small glyphs.
func downscaleToMaxDim(ref *vips.ImageRef, maxDim int) error {
	if maxDim <= 0 {
		return nil
	}
	longest := ref.Width()
	if ref.Height() > longest {
		longest = ref.Height()
	}
	if longest <= maxDim {
		return nil
	}
	scale := float64(maxDim) / float64(longest)
	return ref.Resize(scale, vips.KernelLanczos3)
}

// ─── HTTP transfer (presigned GET / PUT) ──────────────────────────────────────

// fetch GETs the source bytes from a presigned URL, bounded by MaxUploadBytes so
// a lying Content-Length or a chunked stream can't blow past the cap while
// reading. A non-2xx or read failure is ErrFetch (transient). The URL's query
// string (presigned credentials) is redacted from any returned error.
func (e *Engine) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build GET: %v", ErrFetch, redactErr(err))
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: GET: %v", ErrFetch, redactErr(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: GET returned %s", ErrFetch, resp.Status)
	}
	// LimitReader to maxUploadBytes+1: reading one extra byte lets us detect an
	// over-cap payload deterministically regardless of Content-Length honesty.
	limited := io.LimitReader(resp.Body, e.maxUploadBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrFetch, redactErr(err))
	}
	if int64(len(b)) > e.maxUploadBytes {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrOversize, e.maxUploadBytes)
	}
	return b, nil
}

// put uploads the derivative to a presigned PUT URL with the given Content-Type.
// A non-2xx or transport failure is ErrFetch (transient). Content-Length is known
// (the derivative is fully in memory), so this is a plain, non-chunked PUT.
func (e *Engine) put(ctx context.Context, url string, body []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: build PUT: %v", ErrFetch, redactErr(err))
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: PUT: %v", ErrFetch, redactErr(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: PUT returned %s", ErrFetch, resp.Status)
	}
	return nil
}

// ─── Worker pool + idle scavenge (carried from audioengine) ───────────────────

// acquire takes a worker slot or returns the context error if the caller's
// deadline fires first. The matching releaseWorker frees it (and may scavenge).
func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		e.active.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseWorker frees the worker slot and, when this was the last in-flight
// operation, kicks off an idle scavenge so the burst's freed memory returns to
// the OS instead of lingering as container RSS. Only the operation that drains
// the pool to zero triggers it, so a steady stream of work scavenges at most once
// per quiet gap rather than after every call.
func (e *Engine) releaseWorker() {
	idle := e.active.Add(-1) == 0
	<-e.sem
	if idle && e.scavengeOnIdle {
		e.scavenge()
	}
}

// scavenge returns freed memory to the OS off the request path. debug.
// FreeOSMemory runs a stop-the-world GC, so it must not block an operation; it
// also must not pile up if bursts drain to idle repeatedly, hence the
// single-flight guard. Especially worthwhile here: libvips leaves reclaimable
// native pages after a large decode.
func (e *Engine) scavenge() {
	if !e.scavenging.CompareAndSwap(false, true) {
		return // a scavenge is already running
	}
	go func() {
		defer e.scavenging.Store(false)
		e.scavengeFn()
		slog.Debug("returned freed memory to OS after idle", "component", "imgengine.scavenge")
	}()
}

// ─── Header-first format detection + dimension probe ──────────────────────────
//
// The whole point of probeHeader is to know the format and pixel dimensions
// WITHOUT decoding pixels, so the caller can bomb-guard before libvips allocates.
// It is intentionally self-contained (no libvips): it sniffs the format from the
// magic bytes, rejects SVG/vector, enforces the raster allow-list, and reads the
// declared dimensions from the container header. For the ISO-BMFF family
// (HEIC/AVIF) the exact dimensions live deep in the box structure, so a bounded
// header parse extracts them; when they can't be found cheaply, the post-decode
// belt-and-braces guard in loadAndGuard still catches an oversize image.

// probeHeader returns the canonical format name ("jpeg"/"png"/... ) and the
// header-declared width/height. It returns a wrapped Err* sentinel on a vector
// input (ErrVector), an unsupported/unrecognized format (ErrUnsupported), or an
// unreadable header (ErrCorrupt).
func probeHeader(b []byte) (format string, width, height int, err error) {
	if looksLikeSVG(b) {
		return "", 0, 0, fmt.Errorf("%w", ErrVector)
	}
	switch f := sniffRaster(b); f {
	case "jpeg", "png", "gif":
		// stdlib DecodeConfig reads ONLY the header (no pixels) for these formats.
		cfg, _, derr := image.DecodeConfig(bytes.NewReader(b))
		if derr != nil {
			return "", 0, 0, fmt.Errorf("%w: %s header: %v", ErrCorrupt, f, derr)
		}
		return f, cfg.Width, cfg.Height, nil
	case "webp":
		w, h, ok := webpDimensions(b)
		if !ok {
			return "", 0, 0, fmt.Errorf("%w: unreadable webp header", ErrCorrupt)
		}
		return f, w, h, nil
	case "bmp":
		w, h, ok := bmpDimensions(b)
		if !ok {
			return "", 0, 0, fmt.Errorf("%w: unreadable bmp header", ErrCorrupt)
		}
		return f, w, h, nil
	case "tiff":
		w, h, ok := tiffDimensions(b)
		if !ok {
			// TIFF dimensions can be spread across IFD entries in either byte order;
			// if the cheap parse misses them, defer to the post-decode guard rather
			// than reject a valid TIFF.
			return f, 0, 0, nil
		}
		return f, w, h, nil
	case "heic", "avif":
		w, h, ok := isoBMFFDimensions(b)
		if !ok {
			// The ispe box wasn't found in the scanned prefix; defer to the
			// post-decode guard (libvips will still refuse a true bomb on decode).
			return f, 0, 0, nil
		}
		return f, w, h, nil
	default:
		return "", 0, 0, fmt.Errorf("%w: unrecognized or non-image data", ErrUnsupported)
	}
}

// mimeForFormat maps a probeHeader format name to a canonical MIME.
func mimeForFormat(f string) string {
	switch f {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	case "tiff":
		return "image/tiff"
	case "heic":
		return "image/heic"
	case "avif":
		return "image/avif"
	default:
		return "application/octet-stream"
	}
}

// sniffRaster classifies the raster format from magic bytes, returning "" for
// anything off the allow-list. This is the authoritative format decision (the
// allow-list is enforced by returning "" for everything else).
func sniffRaster(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "png"
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return "gif"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "bmp"
	case len(b) >= 4 && (bytes.Equal(b[:4], []byte{'I', 'I', 0x2A, 0x00}) || bytes.Equal(b[:4], []byte{'M', 'M', 0x00, 0x2A})):
		return "tiff"
	case isoBMFFBrand(b) == "heic":
		return "heic"
	case isoBMFFBrand(b) == "avif":
		return "avif"
	default:
		return ""
	}
}

// looksLikeSVG reports whether the leading bytes look like an SVG/XML document.
// SVG can start with an XML prolog, a comment, a BOM, or the <svg root; scan a
// bounded prefix for the "<svg" token to catch all of them without a full parse.
// libvips WILL load and rasterize SVG, so it MUST be rejected before decode.
func looksLikeSVG(b []byte) bool {
	const scan = 1024
	head := b
	if len(head) > scan {
		head = head[:scan]
	}
	lower := bytes.ToLower(head)
	if bytes.Contains(lower, []byte("<svg")) {
		return true
	}
	// Trim leading whitespace and a UTF-8 BOM (0xEF 0xBB 0xBF, spelled as bytes
	// so this source file itself carries no illegal BOM) before the prolog check.
	trimmed := bytes.TrimLeft(lower, " \t\r\n\xef\xbb\xbf")
	return bytes.HasPrefix(trimmed, []byte("<?xml")) && bytes.Contains(lower, []byte("svg"))
}

// isoBMFFBrand returns "heic"/"avif" for an ISO-BMFF (MP4-family) file whose
// major/compatible brand identifies it, else "". HEIC/AVIF share the ftyp box
// structure and differ only by brand.
func isoBMFFBrand(b []byte) string {
	if len(b) < 12 || !bytes.Equal(b[4:8], []byte("ftyp")) {
		return ""
	}
	// Scan the ftyp box's brand list (major brand + compatible brands) within a
	// bounded prefix for a known brand token.
	scan := b
	if len(scan) > 512 {
		scan = scan[:512]
	}
	switch {
	case bytes.Contains(scan, []byte("avif")) || bytes.Contains(scan, []byte("avis")):
		return "avif"
	case bytes.Contains(scan, []byte("heic")) || bytes.Contains(scan, []byte("heix")) ||
		bytes.Contains(scan, []byte("mif1")) || bytes.Contains(scan, []byte("heim")) ||
		bytes.Contains(scan, []byte("hevc")) || bytes.Contains(scan, []byte("msf1")):
		return "heic"
	default:
		return ""
	}
}

// webpDimensions reads width/height from a WebP header (VP8 / VP8L / VP8X
// chunks), header-only. Returns ok=false if the chunk isn't recognized.
func webpDimensions(b []byte) (int, int, bool) {
	if len(b) < 30 || !bytes.Equal(b[:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WEBP")) {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8X": // extended: 24-bit width-1 / height-1 at offset 24
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	case "VP8 ": // lossy: 14-bit dimensions after the 3-byte start code at offset 26
		if len(b) < 30 {
			return 0, 0, false
		}
		w := int(b[26]) | int(b[27])<<8
		h := int(b[28]) | int(b[29])<<8
		return w & 0x3FFF, h & 0x3FFF, true
	case "VP8L": // lossless: 14-bit width-1/height-1 packed after the 0x2F signature
		if len(b) < 25 || b[20] != 0x2F {
			return 0, 0, false
		}
		bits := uint32(b[21]) | uint32(b[22])<<8 | uint32(b[23])<<16 | uint32(b[24])<<24
		w := int(bits&0x3FFF) + 1
		h := int((bits>>14)&0x3FFF) + 1
		return w, h, true
	default:
		return 0, 0, false
	}
}

// bmpDimensions reads width/height from a BMP BITMAPINFOHEADER (little-endian
// int32 at offsets 18 and 22). Height may be negative (top-down bitmap).
func bmpDimensions(b []byte) (int, int, bool) {
	if len(b) < 26 {
		return 0, 0, false
	}
	w := int(int32(binary.LittleEndian.Uint32(b[18:22])))
	h := int(int32(binary.LittleEndian.Uint32(b[22:26])))
	if h < 0 {
		h = -h
	}
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// tiffDimensions reads ImageWidth (tag 0x0100) and ImageLength (tag 0x0101) from
// the first IFD of a TIFF, honoring the file's byte order. Header-only.
func tiffDimensions(b []byte) (int, int, bool) {
	if len(b) < 8 {
		return 0, 0, false
	}
	var bo binary.ByteOrder
	switch {
	case b[0] == 'I' && b[1] == 'I':
		bo = binary.LittleEndian
	case b[0] == 'M' && b[1] == 'M':
		bo = binary.BigEndian
	default:
		return 0, 0, false
	}
	ifdOff := int(bo.Uint32(b[4:8]))
	if ifdOff <= 0 || ifdOff+2 > len(b) {
		return 0, 0, false
	}
	count := int(bo.Uint16(b[ifdOff : ifdOff+2]))
	var w, h int
	for i := 0; i < count; i++ {
		off := ifdOff + 2 + i*12
		if off+12 > len(b) {
			break
		}
		tag := bo.Uint16(b[off : off+2])
		typ := bo.Uint16(b[off+2 : off+4])
		val := b[off+8 : off+12]
		read := func() int {
			if typ == 3 { // SHORT
				return int(bo.Uint16(val[:2]))
			}
			return int(bo.Uint32(val)) // LONG
		}
		switch tag {
		case 0x0100:
			w = read()
		case 0x0101:
			h = read()
		}
	}
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// isoBMFFDimensions extracts width/height from an ISO-BMFF (HEIC/AVIF) file's
// ispe (image spatial extents) box. The box layout is nested and version-
// dependent, so rather than a full box walk this scans a bounded prefix for the
// "ispe" marker and reads the two big-endian uint32 dimensions that follow its
// 4-byte version/flags field. Best-effort: ok=false when not found in the prefix.
func isoBMFFDimensions(b []byte) (int, int, bool) {
	scan := b
	const limit = 64 << 10 // ispe lives in the meta box near the file start
	if len(scan) > limit {
		scan = scan[:limit]
	}
	idx := bytes.Index(scan, []byte("ispe"))
	if idx < 0 {
		return 0, 0, false
	}
	// bytes.Index points at the "ispe" box TYPE (4 bytes). The FullBox layout is:
	//   [type "ispe"(4)] [version(1)+flags(3)] [width uint32 BE] [height uint32 BE]
	// so the dimensions start 8 bytes past the marker (4 type + 4 version/flags).
	// The previous code used idx+4, reading the version/flags word as the width and
	// the width as the height — so it never returned real dimensions.
	p := idx + 8
	if p+8 > len(scan) {
		return 0, 0, false
	}
	w := int(binary.BigEndian.Uint32(scan[p : p+4]))
	h := int(binary.BigEndian.Uint32(scan[p+4 : p+8]))
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// ─── Misc helpers ─────────────────────────────────────────────────────────────

// presignedQueryRe matches the query string of an http(s) URL — where presigned
// credentials (signatures, access keys, tokens) live. net/http embeds the request
// URL in its error messages, so the query is stripped before an error is wrapped
// into a gRPC response. The full URL is only ever exposed via server-side
// logging, never to the client.
var presignedQueryRe = regexp.MustCompile(`(https?://[^\s?]+)\?\S+`)

// redactURLCredentials removes URL query strings from a diagnostic message,
// leaving the rest of the text intact.
func redactURLCredentials(s string) string {
	return presignedQueryRe.ReplaceAllString(s, "${1}?<redacted>")
}

// redactErr strips presigned query strings from an error's message.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", redactURLCredentials(err.Error()))
}

// vipsSlogBridge routes libvips' internal log messages through slog so native
// warnings are structured (component=imgengine.vips) instead of hitting stderr
// raw — the libvips analog of the gRPC framework bridge.
func vipsSlogBridge(domain string, level vips.LogLevel, msg string) {
	attrs := []any{"component", "imgengine.vips", "domain", domain}
	switch level {
	case vips.LogLevelError, vips.LogLevelCritical:
		slog.Error(msg, attrs...)
	case vips.LogLevelWarning:
		slog.Warn(msg, attrs...)
	default:
		slog.Debug(msg, attrs...)
	}
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orInt64(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
