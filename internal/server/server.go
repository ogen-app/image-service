// Package server implements the image.v1.ImageService gRPC service on top of the
// libvips engine (imgengine) and the Gemini vision client (vision). It is the
// thin orchestration layer: it validates requests, drives the engine + model,
// maps domain errors to gRPC status codes (terminal InvalidArgument vs transient
// Unavailable/Internal/DeadlineExceeded), and shapes the proto responses.
//
// Two ingress paths (decision D6 — image-service is the sole image ingress):
//
//   - Extract           — normalize (imgengine) → classify+extract+describe+alt
//     (vision) → respond with Blocks + description + alt text + normalized meta +
//     per-call token usage. The full content-bank pipeline.
//   - PrepareAttachment — validate + metadata + EXIF/geo strip (pixels preserved,
//     imgengine) + optional alt text (vision). No structured extraction.
//
// The vision client is key-optional: with no GEMINI_API_KEY the metadata paths
// (PrepareAttachment without alt text) still serve, and any model-requiring RPC
// returns Unavailable — the same graceful-degrade contract as audio-service.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/ogen-app/image-service/gen/documents/v1"
	imagev1 "github.com/ogen-app/image-service/gen/image/v1"
	"github.com/ogen-app/image-service/internal/imgengine"
	"github.com/ogen-app/image-service/internal/vision"
)

// Vision is the subset of the Gemini client the server needs, narrowed to an
// interface so tests can substitute a fake without a real API key (mirrors
// audio-service's Transcriber seam).
type Vision interface {
	Available() bool
	Extract(ctx context.Context, img vision.Image) (*vision.ExtractResult, error)
	GenerateAltText(ctx context.Context, imgBytes []byte, mime, model string, maxChars int) (string, bool, vision.Usage, error)
}

// Defaults carries the config-supplied fallbacks used when a request leaves a
// model id / cap / threshold empty. Model ids are never compiled in; these come
// from config and the request always wins.
type Defaults struct {
	ClassifyModel       string
	ExtractModel        string
	EscalateModel       string
	AltTextModel        string
	AltTextMaxChars     int
	ConfidenceThreshold float64
}

// Server implements imagev1.ImageServiceServer.
type Server struct {
	imagev1.UnimplementedImageServiceServer
	engine *imgengine.Engine
	vis    Vision
	def    Defaults
}

// New wires the server with its engine, vision client, and config defaults.
func New(engine *imgengine.Engine, vis Vision, def Defaults) *Server {
	return &Server{engine: engine, vis: vis, def: def}
}

// Extract runs the full content-bank pipeline. It requires a Gemini key (the
// pipeline is vision-driven); with none it returns Unavailable. A terminal
// content verdict from the engine (SVG/corrupt/oversize/unsupported) is returned
// as InvalidArgument with rejected_reason set on the response would be
// inconsistent with a gRPC error, so terminal verdicts surface purely as the
// status error (rejected_reason stays empty in the success path).
func (s *Server) Extract(ctx context.Context, req *imagev1.ExtractRequest) (*imagev1.ExtractResponse, error) {
	src := strings.TrimSpace(req.GetSourceUrl())
	if src == "" {
		return nil, status.Error(codes.InvalidArgument, "source_url is required")
	}
	if !s.vis.Available() {
		return nil, status.Error(codes.Unavailable, "vision backend not configured (no gemini api key)")
	}

	// 1) Normalize + validate + bomb-guard via libvips. A non-text image is
	// downscaled for storage, but we don't yet know the shape — so produce the
	// full-resolution derivative and let the vision pass read it; the STORED asset
	// downscaling decision is orthogonal and left to ogen for v1 (downscaleNonText
	// stays false here so text is never lost before classification).
	eng, err := s.engine.Extract(ctx, src, strings.TrimSpace(req.GetDestPutUrl()), req.GetSourceNormalized(), false)
	if err != nil {
		return nil, mapEngineErr(err)
	}

	// 2) Run the vision pipeline over the SAME derivative bytes (no re-fetch).
	vres, err := s.vis.Extract(ctx, vision.Image{
		Bytes:            eng.Bytes,
		MIME:             eng.DerivedMIME,
		ClassifyModel:    orStr(req.GetClassifyModel(), s.def.ClassifyModel),
		ExtractModel:     orStr(req.GetExtractModel(), s.def.ExtractModel),
		EscalateModel:    orStr(req.GetEscalateModel(), s.def.EscalateModel),
		DescribeModel:    orStr(req.GetExtractModel(), s.def.ExtractModel),
		AltTextModel:     orStr(req.GetExtractModel(), s.def.AltTextModel),
		AltTextMaxChars:  orInt(int(req.GetAltTextMaxChars()), s.def.AltTextMaxChars),
		ConfidenceThresh: orFloat(float64(req.GetConfidenceThreshold()), s.def.ConfidenceThreshold),
	})
	if err != nil {
		return nil, mapVisionErr(err)
	}

	resp := &imagev1.ExtractResponse{
		Shape:              toProtoShape(vres.Shape),
		ClassifyConfidence: vres.ClassifyConfidence,
		Blocks:             toProtoBlocks(vres.Blocks),
		Description:        vres.Description,
		AltText:            vres.AltText,
		Normalized: &imagev1.NormalizedMeta{
			Mime:           eng.DerivedMIME,
			Width:          int32(eng.DerivedWidth),
			Height:         int32(eng.DerivedHeight),
			IsAnimated:     eng.Meta.IsAnimated,
			ChecksumSha256: eng.Meta.SHA256,
		},
		Escalated:          vres.Escalated,
		EscalationImproved: vres.EscalationImproved,
		DescriptionOk:      vres.DescriptionOK,
		ExtractionOk:       vres.ExtractionOK,
		Truncated:          vres.Truncated,
		Usage:              toProtoUsage(vres.Usage),
	}

	slog.InfoContext(ctx, "extract complete",
		"component", "server.extract",
		"filename", req.GetFilename(),
		"shape", vres.Shape.String(),
		"classify_confidence", vres.ClassifyConfidence,
		"blocks", len(vres.Blocks),
		"escalated", vres.Escalated,
		"escalation_improved", vres.EscalationImproved,
		"description_ok", vres.DescriptionOK,
		"extraction_ok", vres.ExtractionOK,
		"derived_mime", eng.DerivedMIME,
		"usage_calls", len(vres.Usage),
	)
	return resp, nil
}

// PrepareAttachment runs the light path: validate + metadata + EXIF/geo strip
// (pixels preserved) + optional alt text. It does NOT require a Gemini key unless
// want_alt_text is set.
//
// Metadata is stripped UNCONDITIONALLY: EXIF/geolocation stripping is a non-
// negotiable privacy invariant for every ingested image (CON-281 §7/§17), and the
// proto strip_metadata field defaults false on the wire — honoring that default
// would silently publish geotagged photos. So the request flag is ignored here
// and we always strip. (Animated inputs are preserved frame-for-frame by the
// engine, which passes them through rather than flattening — see imgengine.)
func (s *Server) PrepareAttachment(ctx context.Context, req *imagev1.PrepareAttachmentRequest) (*imagev1.PrepareAttachmentResponse, error) {
	src := strings.TrimSpace(req.GetSourceUrl())
	if src == "" {
		return nil, status.Error(codes.InvalidArgument, "source_url is required")
	}

	// Always strip (privacy invariant) — do NOT thread req.GetStripMetadata().
	pres, err := s.engine.PrepareAttachment(ctx, src, strings.TrimSpace(req.GetDestPutUrl()), true)
	if err != nil {
		return nil, mapEngineErr(err)
	}

	// Report the DERIVATIVE (what ogen stores + publishes): its MIME and byte size,
	// but the ORIGINAL's checksum (ogen dedupes on it) and pixel dimensions (pixels
	// are preserved, so dims are unchanged).
	resp := &imagev1.PrepareAttachmentResponse{
		Mime:           pres.DerivedMIME,
		SizeBytes:      int64(len(pres.Bytes)),
		Width:          int32(pres.Meta.Width),
		Height:         int32(pres.Meta.Height),
		IsAnimated:     pres.Meta.IsAnimated,
		FrameCount:     int32(pres.Meta.FrameCount),
		ChecksumSha256: pres.Meta.SHA256,
	}

	// Optional alt text — the only step here that needs the model.
	if req.GetWantAltText() {
		if !s.vis.Available() {
			return nil, status.Error(codes.Unavailable, "alt text requested but vision backend not configured (no gemini api key)")
		}
		model := orStr(req.GetAltTextModel(), s.def.AltTextModel)
		maxChars := orInt(int(req.GetAltTextMaxChars()), s.def.AltTextMaxChars)
		// Run alt text over the stripped derivative bytes (same pixels) + its MIME,
		// not a re-fetch of the source.
		alt, _, usage, aerr := s.vis.GenerateAltText(ctx, pres.Bytes, pres.DerivedMIME, model, maxChars)
		if aerr != nil {
			return nil, mapVisionErr(aerr)
		}
		resp.AltText = alt
		resp.Usage = toProtoUsage([]vision.Usage{usage})
	}

	slog.InfoContext(ctx, "prepare attachment complete",
		"component", "server.prepare",
		"filename", req.GetFilename(),
		"mime", pres.DerivedMIME,
		"width", pres.Meta.Width,
		"height", pres.Meta.Height,
		"is_animated", pres.Meta.IsAnimated,
		"stripped", true,
		"alt_text", req.GetWantAltText(),
	)
	return resp, nil
}

// GenerateAltText (re)generates alt text for an existing image. Requires a Gemini
// key. It fetches + validates the image via the engine's PrepareAttachment path
// with stripping ON (dest empty → no PUT) so the same bomb guards apply AND the
// bytes handed to Gemini are EXIF/geo-stripped — the model must never receive the
// user's original metadata (CON-281 §17). It then runs the alt-text model over
// the stripped derivative.
func (s *Server) GenerateAltText(ctx context.Context, req *imagev1.GenerateAltTextRequest) (*imagev1.GenerateAltTextResponse, error) {
	src := strings.TrimSpace(req.GetSourceUrl())
	if src == "" {
		return nil, status.Error(codes.InvalidArgument, "source_url is required")
	}
	if !s.vis.Available() {
		return nil, status.Error(codes.Unavailable, "vision backend not configured (no gemini api key)")
	}

	// Fetch + validate + bomb-guard + STRIP metadata (no PUT — dest empty). The
	// stripped derivative is what Gemini sees.
	pres, err := s.engine.PrepareAttachment(ctx, src, "", true)
	if err != nil {
		return nil, mapEngineErr(err)
	}

	model := orStr(req.GetModel(), s.def.AltTextModel)
	maxChars := orInt(int(req.GetMaxChars()), s.def.AltTextMaxChars)
	alt, _, usage, aerr := s.vis.GenerateAltText(ctx, pres.Bytes, pres.DerivedMIME, model, maxChars)
	if aerr != nil {
		return nil, mapVisionErr(aerr)
	}

	slog.InfoContext(ctx, "generate alt text complete",
		"component", "server.alttext",
		"mime", pres.DerivedMIME,
		"model", model,
		"chars", len([]rune(alt)),
	)
	return &imagev1.GenerateAltTextResponse{
		AltText: alt,
		Usage:   toProtoUsage([]vision.Usage{usage}),
	}, nil
}

// ─── Error mapping ─────────────────────────────────────────────────────────────

// mapEngineErr classifies an imgengine error into a gRPC status. Terminal content
// verdicts (unsupported / vector / corrupt / oversize) are InvalidArgument so the
// client does not retry. A presigned-transfer failure is Internal (transient). A
// context deadline/cancel surfaces as its own code so callers see the timeout.
func mapEngineErr(err error) error {
	switch {
	case errors.Is(err, imgengine.ErrUnsupported),
		errors.Is(err, imgengine.ErrVector),
		errors.Is(err, imgengine.ErrCorrupt),
		errors.Is(err, imgengine.ErrOversize):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, imgengine.ErrFetch):
		return status.Error(codes.Internal, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// mapVisionErr classifies a Gemini vision error. An unconfigured key is
// Unavailable; HTTP 429 / 5xx map to ResourceExhausted / Unavailable (transient);
// a deadline surfaces as DeadlineExceeded. A malformed request to Gemini is our
// bug, surfaced as Internal rather than pushed back as InvalidArgument. Mirrors
// audio-service's mapTranscribeErr.
func mapVisionErr(err error) error {
	if errors.Is(err, vision.ErrUnavailable) {
		return status.Error(codes.Unavailable, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	switch code := geminiHTTPStatus(err); {
	case code == http.StatusTooManyRequests:
		return status.Error(codes.ResourceExhausted, err.Error())
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		// 408/504 are timeouts (check before the general 5xx range so 504 doesn't
		// fall through to Unavailable).
		return status.Error(codes.DeadlineExceeded, err.Error())
	case code >= 500 && code <= 599:
		return status.Error(codes.Unavailable, err.Error())
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		return status.Error(codes.Internal, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// geminiHTTPStatus extracts the HTTP status code from a genai.APIError, or 0 if
// the error isn't one. genai.APIError is a value type, so errors.As targets a
// non-pointer variable of it.
func geminiHTTPStatus(err error) int {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}

// ─── proto mapping ─────────────────────────────────────────────────────────────

func toProtoShape(s vision.Shape) imagev1.Shape {
	switch s {
	case vision.ShapeProse:
		return imagev1.Shape_SHAPE_PROSE
	case vision.ShapeConversation:
		return imagev1.Shape_SHAPE_CONVERSATION
	case vision.ShapeSocialPost:
		return imagev1.Shape_SHAPE_SOCIAL_POST
	case vision.ShapeTabular:
		return imagev1.Shape_SHAPE_TABULAR
	case vision.ShapeCreative:
		return imagev1.Shape_SHAPE_CREATIVE
	default:
		return imagev1.Shape_SHAPE_UNSPECIFIED
	}
}

func toProtoBlocks(blocks []vision.Block) []*imagev1.Block {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]*imagev1.Block, 0, len(blocks))
	for _, b := range blocks {
		pb := &imagev1.Block{
			Kind:          b.Kind,
			Level:         int32(b.Level),
			Text:          b.Text,
			Provenance:    b.Provenance,
			LowConfidence: b.LowConfidence,
		}
		for _, c := range b.Cells {
			pb.Cells = append(pb.Cells, &imagev1.Cell{Row: int32(c.Row), Col: int32(c.Col), Text: c.Text})
		}
		if b.Bbox != nil {
			// image.v1.Block.anchor is the SHARED documents.v1.Anchor (CON-220):
			// kind = ANCHOR_KIND_IMAGE_REGION + the normalized [0,1] bbox. The
			// vision package keeps its own internal Bbox struct; this is the only
			// place it's mapped onto the proto citation shape.
			pb.Anchor = &documentsv1.Anchor{
				Kind: documentsv1.AnchorKind_ANCHOR_KIND_IMAGE_REGION,
				Bbox: &documentsv1.Bbox{X: b.Bbox.X, Y: b.Bbox.Y, W: b.Bbox.W, H: b.Bbox.H},
			}
		}
		out = append(out, pb)
	}
	return out
}

func toProtoUsage(usage []vision.Usage) []*imagev1.TokenUsage {
	if len(usage) == 0 {
		return nil
	}
	out := make([]*imagev1.TokenUsage, 0, len(usage))
	for _, u := range usage {
		out = append(out, &imagev1.TokenUsage{
			Model:  u.Model,
			Step:   u.Step,
			Input:  u.Input,
			Output: u.Output,
		})
	}
	return out
}

func orStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}
