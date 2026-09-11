// Package vision runs the Gemini vision model over an image via the Gemini
// Developer API (decision D1 — the model runs IN this service, D2 — Gemini
// Developer API, not Vertex, in v1). It is the image-service analog of
// audio-service's transcribe package: deliberately thin, it sends image bytes to
// Gemini with structured-output prompts, parses the JSON replies, and reports
// per-call token usage so ogen can price it via its `gemini` vendor (CON-86).
//
// The pipeline has four steps, each a separate model call so ogen sees per-step
// cost and so a partial failure (e.g. description fails, extraction succeeds)
// still yields usable output:
//
//   - Classify  — LOW media_resolution pass: coarse Shape + a 0..1 confidence.
//     Cheap; picks which per-shape extraction prompt/schema to use.
//   - Extract   — HIGH media_resolution pass: per-shape structured Blocks (the
//     high resolution lets the model read small text a low-res pass would miss).
//   - Describe  — a long, embeddable prose description (ogen embeds it as text;
//     raw-image embeddings are deferred, decision D3).
//   - GenerateAltText — a short accessibility string (decision D4 — shipped by
//     default; ogen protects user edits with alt_text_edited_by_user).
//
// Escalation (decision — escalate once): when Classify or Extract reports
// confidence below the threshold, that step re-runs EXACTLY ONCE on a stronger
// escalate model. The outcome is recorded regardless of whether it improved.
//
// Model ids are ALWAYS supplied by the caller (server, from request/config) and
// NEVER compiled in — so ogen can swap models without redeploying the service.
//
// EU data residency is DEFERRED (decision D2, shared with audio-service): v1 uses
// the Gemini Developer API; a later revision swaps the genai Backend to Vertex AI
// in europe-west without touching the RPC contract.
package vision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// ErrUnavailable is returned when no Gemini API key is configured, so the vision
// calls cannot run. The server maps it to codes.Unavailable (transient: a key can
// be set without a restart) — mirrors audio-service's transcribe.ErrUnavailable.
var ErrUnavailable = errors.New("vision: gemini api key not configured")

// Shape mirrors image.v1.Shape. Kept as a local type so the vision package has no
// dependency on the generated proto; the server maps between them.
type Shape int

const (
	ShapeUnspecified Shape = iota
	ShapeProse
	ShapeConversation
	ShapeSocialPost
	ShapeTabular
	ShapeCreative
)

// String is the canonical wire token the model is asked to emit and that
// parseShape decodes.
func (s Shape) String() string {
	switch s {
	case ShapeProse:
		return "prose"
	case ShapeConversation:
		return "conversation"
	case ShapeSocialPost:
		return "social_post"
	case ShapeTabular:
		return "tabular"
	case ShapeCreative:
		return "creative"
	default:
		return "unspecified"
	}
}

// parseShape decodes a model-emitted shape token, defaulting to creative (the
// safest catch-all: it produces a description + alt text with no structural
// claims) when the token is unknown.
func parseShape(s string) Shape {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "prose":
		return ShapeProse
	case "conversation":
		return ShapeConversation
	case "social_post", "social", "post":
		return ShapeSocialPost
	case "tabular", "table", "chart":
		return ShapeTabular
	case "creative", "photo", "image", "illustration":
		return ShapeCreative
	default:
		return ShapeCreative
	}
}

// Usage is one model call's token accounting; the server maps it to
// image.v1.TokenUsage. step names the pipeline stage for cost attribution.
type Usage struct {
	Model  string
	Step   string
	Input  int64
	Output int64
}

// Block is one structured unit of extracted content (mirrors image.v1.Block; the
// server maps between them). Cells carries a table grid; Bbox optionally
// localizes the block.
type Block struct {
	Kind          string
	Level         int
	Text          string
	Cells         []Cell
	Bbox          *Bbox
	Provenance    string
	LowConfidence bool
}

// Cell is one cell of a tabular Block (0-based row/col).
type Cell struct {
	Row  int
	Col  int
	Text string
}

// Bbox is a normalized [0,1] bounding box (top-left origin).
type Bbox struct {
	X, Y, W, H float32
}

// Image is the input to the pipeline: the (normalized) image bytes and their
// MIME, plus the model ids and knobs for this run. All model ids come from the
// caller.
type Image struct {
	Bytes            []byte
	MIME             string
	ClassifyModel    string
	ExtractModel     string
	EscalateModel    string
	DescribeModel    string // usually == ExtractModel; server passes ExtractModel
	AltTextModel     string
	AltTextMaxChars  int
	ConfidenceThresh float64
}

// ExtractResult is the full pipeline output: the classified shape, its
// confidence, per-shape Blocks, the description and alt text, escalation flags,
// per-step *_ok flags, and the accumulated per-call usage.
type ExtractResult struct {
	Shape              Shape
	ClassifyConfidence float32
	Blocks             []Block
	Description        string
	AltText            string
	Escalated          bool
	EscalationImproved bool
	DescriptionOK      bool
	ExtractionOK       bool
	Truncated          bool
	Usage              []Usage
}

// Client wraps a genai.Client for the Gemini Developer API backend. A nil-inner
// Client (no key) reports Available()==false and every call returns
// ErrUnavailable — matching audio-service's key-optional boot so
// PrepareAttachment's metadata path still serves without a key.
type Client struct {
	inner *genai.Client
}

// New builds a Gemini Developer API client from apiKey. An empty key yields a
// nil-inner Client. To move to Vertex/EU later, switch Backend to
// genai.BackendVertexAI and set a location (the SDK supports both) — no other
// code here changes.
func New(ctx context.Context, apiKey string) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return &Client{}, nil
	}
	inner, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("vision: new gemini client: %w", err)
	}
	return &Client{inner: inner}, nil
}

// Available reports whether a Gemini key was configured at construction.
func (c *Client) Available() bool { return c != nil && c.inner != nil }

// Extract runs the full four-step pipeline with one-shot escalation. Gemini
// transport errors from the CLASSIFY step propagate (the server classifies them
// as transient); a failure in a LATER step is tolerated — its *_ok flag is left
// false and the partial result returns with whatever succeeded, so ogen keeps the
// spend already incurred. Usage accumulates across every call made.
func (c *Client) Extract(ctx context.Context, img Image) (*ExtractResult, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	if len(img.Bytes) == 0 {
		return nil, fmt.Errorf("vision: empty image bytes")
	}
	thresh := img.ConfidenceThresh
	if thresh <= 0 {
		thresh = 0.6
	}

	res := &ExtractResult{}

	// 1) Classify (low resolution). A hard failure here aborts — without a shape
	// the extract prompt can't be chosen, and a classify failure is usually a
	// transport fault the caller should retry.
	shape, conf, u, err := c.classify(ctx, img, img.ClassifyModel, mediaLow)
	res.Usage = append(res.Usage, u)
	if err != nil {
		return nil, err
	}
	res.Shape, res.ClassifyConfidence = shape, conf

	// Escalate the classification once if the model was unsure.
	if float64(conf) < thresh && img.EscalateModel != "" {
		res.Escalated = true
		eShape, eConf, eu, eErr := c.classify(ctx, img, img.EscalateModel, mediaLow)
		res.Usage = append(res.Usage, eu)
		if eErr == nil && eConf > conf {
			res.Shape, res.ClassifyConfidence = eShape, eConf
			res.EscalationImproved = true
			shape = eShape
		}
	}

	// 2) Extract (high resolution), per-shape prompt/schema.
	blocks, exConf, xu, xErr := c.extract(ctx, img, shape, img.ExtractModel)
	res.Usage = append(res.Usage, xu)
	if xErr == nil {
		res.Blocks = blocks
		res.ExtractionOK = true
		// Escalate the extraction once if it came back low-confidence and we haven't
		// already spent an escalation on classify (escalate ONCE total across the
		// run, per the decision — pick the step that was actually unsure).
		if float64(exConf) < thresh && img.EscalateModel != "" && !res.Escalated {
			res.Escalated = true
			eBlocks, eConf, eu, eErr := c.extract(ctx, img, shape, img.EscalateModel)
			res.Usage = append(res.Usage, eu)
			if eErr == nil && eConf > exConf {
				res.Blocks = eBlocks
				res.EscalationImproved = true
			}
		}
	}
	// Tabular is inherently lossy from a screenshot — mark every tabular block low
	// confidence so ogen surfaces a "supply the source file" hint (decision).
	if shape == ShapeTabular {
		for i := range res.Blocks {
			res.Blocks[i].LowConfidence = true
		}
	}

	// 3) Describe (embeddable prose). Best-effort — a failure leaves DescriptionOK
	// false without failing the whole Extract.
	desc, du, dErr := c.describe(ctx, img, orModel(img.DescribeModel, img.ExtractModel))
	res.Usage = append(res.Usage, du)
	if dErr == nil {
		res.Description = desc
		res.DescriptionOK = true
	}

	// 4) Alt text (short accessibility string). Best-effort.
	alt, truncated, au, aErr := c.altText(ctx, img.Bytes, img.MIME, orModel(img.AltTextModel, img.ExtractModel), img.AltTextMaxChars)
	res.Usage = append(res.Usage, au)
	if aErr == nil {
		res.AltText = alt
		res.Truncated = truncated
	}

	return res, nil
}

// GenerateAltText is the standalone alt-text RPC's backend: one model call,
// returning the string, a truncation flag, and usage.
func (c *Client) GenerateAltText(ctx context.Context, imgBytes []byte, mime, model string, maxChars int) (string, bool, Usage, error) {
	if !c.Available() {
		return "", false, Usage{}, ErrUnavailable
	}
	if len(imgBytes) == 0 {
		return "", false, Usage{}, fmt.Errorf("vision: empty image bytes")
	}
	return c.altText(ctx, imgBytes, mime, model, maxChars)
}

// ─── Steps ────────────────────────────────────────────────────────────────────

// mediaResolution selects Gemini's media_resolution: low for the cheap classify
// pass, high so the extract pass can read small text. Kept as a small enum so the
// two call sites read clearly.
type mediaResolution int

const (
	mediaLow mediaResolution = iota
	mediaHigh
)

func (m mediaResolution) genai() genai.MediaResolution {
	if m == mediaHigh {
		return genai.MediaResolutionHigh
	}
	return genai.MediaResolutionLow
}

// classify runs the low-res classification pass and returns the shape and a 0..1
// confidence. model is the id to use (base or escalate).
func (c *Client) classify(ctx context.Context, img Image, model string, res mediaResolution) (Shape, float32, Usage, error) {
	if strings.TrimSpace(model) == "" {
		return ShapeUnspecified, 0, Usage{}, fmt.Errorf("vision: empty classify model")
	}
	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"shape":      {Type: genai.TypeString, Enum: []string{"prose", "conversation", "social_post", "tabular", "creative"}},
			"confidence": {Type: genai.TypeNumber},
		},
		Required: []string{"shape", "confidence"},
	}
	cfg := c.jsonConfig(schema, res)
	resp, err := c.generate(ctx, model, classifyPrompt(), img.Bytes, img.MIME, cfg)
	in, out := usageTokens(resp)
	u := Usage{Model: model, Step: "classify", Input: in, Output: out}
	if err != nil {
		return ShapeUnspecified, 0, u, err
	}
	var parsed struct {
		Shape      string  `json:"shape"`
		Confidence float32 `json:"confidence"`
	}
	if perr := decodeJSON(resp, &parsed); perr != nil {
		// The model replied but the JSON is unusable. A classification we can't read
		// defaults to creative with zero confidence (which will trigger escalation);
		// don't fail the call on a parse hiccup.
		return ShapeCreative, 0, u, nil
	}
	return parseShape(parsed.Shape), clamp01(parsed.Confidence), u, nil
}

// extract runs the high-res per-shape structured extraction and returns the
// blocks plus an overall extraction confidence.
func (c *Client) extract(ctx context.Context, img Image, shape Shape, model string) ([]Block, float32, Usage, error) {
	if strings.TrimSpace(model) == "" {
		return nil, 0, Usage{}, fmt.Errorf("vision: empty extract model")
	}
	cfg := c.jsonConfig(extractSchema(), mediaHigh)
	resp, err := c.generate(ctx, model, extractPrompt(shape), img.Bytes, img.MIME, cfg)
	in, out := usageTokens(resp)
	u := Usage{Model: model, Step: "extract", Input: in, Output: out}
	if err != nil {
		return nil, 0, u, err
	}
	var parsed extractReply
	if perr := decodeJSON(resp, &parsed); perr != nil {
		return nil, 0, u, fmt.Errorf("vision: decode extract reply: %w", perr)
	}
	return parsed.toBlocks(), clamp01(parsed.Confidence), u, nil
}

// describe runs the embeddable-description pass (free-form text, no schema).
func (c *Client) describe(ctx context.Context, img Image, model string) (string, Usage, error) {
	if strings.TrimSpace(model) == "" {
		return "", Usage{}, fmt.Errorf("vision: empty describe model")
	}
	resp, err := c.generate(ctx, model, describePrompt(), img.Bytes, img.MIME, c.textConfig(mediaHigh))
	in, out := usageTokens(resp)
	u := Usage{Model: model, Step: "describe", Input: in, Output: out}
	if err != nil {
		return "", u, err
	}
	return strings.TrimSpace(textOf(resp)), u, nil
}

// altText runs the short-alt-text pass and enforces the char cap. It returns the
// (possibly truncated) alt text and whether truncation happened.
func (c *Client) altText(ctx context.Context, imgBytes []byte, mime, model string, maxChars int) (string, bool, Usage, error) {
	if strings.TrimSpace(model) == "" {
		return "", false, Usage{}, fmt.Errorf("vision: empty alt text model")
	}
	if maxChars <= 0 {
		maxChars = 420
	}
	resp, err := c.generate(ctx, model, altTextPrompt(maxChars), imgBytes, mime, c.textConfig(mediaLow))
	in, out := usageTokens(resp)
	u := Usage{Model: model, Step: "alt_text", Input: in, Output: out}
	if err != nil {
		return "", false, u, err
	}
	alt := collapseWhitespace(textOf(resp))
	truncated := false
	// Enforce the cap on RUNES, not bytes, so a multibyte glyph isn't split.
	if r := []rune(alt); len(r) > maxChars {
		alt = strings.TrimSpace(string(r[:maxChars]))
		truncated = true
	}
	return alt, truncated, u, nil
}

// ─── genai plumbing ───────────────────────────────────────────────────────────

// generate is the single call site into the SDK: it builds the multimodal content
// (prompt text + image part), runs GenerateContent, and returns the raw response.
// Keeping it in one place means the image-part construction, the deterministic
// temperature, and the media-resolution wiring are consistent across steps.
func (c *Client) generate(ctx context.Context, model, prompt string, imgBytes []byte, mime string, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
		genai.NewPartFromBytes(imgBytes, orMIME(mime)),
	}
	contents := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}
	return c.inner.Models.GenerateContent(ctx, model, contents, cfg)
}

// jsonConfig builds a structured-output config: JSON response constrained by
// schema, deterministic temperature, and the media resolution for the pass.
func (c *Client) jsonConfig(schema *genai.Schema, res mediaResolution) *genai.GenerateContentConfig {
	return &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema:   schema,
		Temperature:      genai.Ptr[float32](0),
		MediaResolution:  res.genai(),
	}
}

// textConfig builds a free-text config (used by describe/alt-text) with a low
// temperature for stable, factual output.
func (c *Client) textConfig(res mediaResolution) *genai.GenerateContentConfig {
	return &genai.GenerateContentConfig{
		Temperature:     genai.Ptr[float32](0.2),
		MediaResolution: res.genai(),
	}
}

// usageTokens pulls Gemini's prompt/candidate token counts, tolerating a nil
// response or UsageMetadata (a failed call still returns zeroed usage so the step
// is accounted).
func usageTokens(resp *genai.GenerateContentResponse) (in, out int64) {
	if resp == nil || resp.UsageMetadata == nil {
		return 0, 0
	}
	return int64(resp.UsageMetadata.PromptTokenCount), int64(resp.UsageMetadata.CandidatesTokenCount)
}

// textOf returns the response's text, tolerating a nil response.
func textOf(resp *genai.GenerateContentResponse) string {
	if resp == nil {
		return ""
	}
	return resp.Text()
}

// decodeJSON unmarshals the response text into v, tolerating a code-fence wrapper
// some models still emit despite responseMIMEType=application/json.
func decodeJSON(resp *genai.GenerateContentResponse, v any) error {
	s := stripCodeFence(strings.TrimSpace(textOf(resp)))
	if s == "" {
		return fmt.Errorf("vision: empty model reply")
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("vision: decode model reply: %w", err)
	}
	return nil
}

// stripCodeFence removes a leading ```json / ``` fence and trailing ``` if the
// model wrapped its JSON in one.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func orMIME(m string) string {
	if strings.TrimSpace(m) == "" {
		return "image/png"
	}
	return m
}

func orModel(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func clamp01(f float32) float32 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// collapseWhitespace flattens runs of whitespace to single spaces and trims —
// alt text must be a single clean line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ─── Structured-output schema + prompts ───────────────────────────────────────

// extractReply is the shape decodeJSON parses the extract pass into.
type extractReply struct {
	Confidence float32     `json:"confidence"`
	Blocks     []jsonBlock `json:"blocks"`
}

type jsonBlock struct {
	Kind          string     `json:"kind"`
	Level         int        `json:"level"`
	Text          string     `json:"text"`
	Cells         []jsonCell `json:"cells"`
	Bbox          *jsonBbox  `json:"bbox"`
	LowConfidence bool       `json:"low_confidence"`
}

type jsonCell struct {
	Row  int    `json:"row"`
	Col  int    `json:"col"`
	Text string `json:"text"`
}

type jsonBbox struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	W float32 `json:"w"`
	H float32 `json:"h"`
}

// toBlocks maps the parsed JSON blocks into domain Blocks, stamping provenance
// "extract" (the caller flips it to escalate/tabular as needed).
func (r extractReply) toBlocks() []Block {
	out := make([]Block, 0, len(r.Blocks))
	for _, b := range r.Blocks {
		blk := Block{
			Kind:          strings.TrimSpace(b.Kind),
			Level:         b.Level,
			Text:          b.Text,
			Provenance:    "extract",
			LowConfidence: b.LowConfidence,
		}
		for _, cl := range b.Cells {
			blk.Cells = append(blk.Cells, Cell{Row: cl.Row, Col: cl.Col, Text: cl.Text})
		}
		if b.Bbox != nil {
			blk.Bbox = &Bbox{X: clamp01(b.Bbox.X), Y: clamp01(b.Bbox.Y), W: clamp01(b.Bbox.W), H: clamp01(b.Bbox.H)}
		}
		out = append(out, blk)
	}
	return out
}

// extractSchema constrains the extract pass output. The same schema serves every
// shape — the PROMPT tells the model which kinds to use — so the response parser
// stays uniform.
func extractSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"confidence": {Type: genai.TypeNumber},
			"blocks": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"kind":  {Type: genai.TypeString},
						"level": {Type: genai.TypeInteger},
						"text":  {Type: genai.TypeString},
						"cells": {
							Type: genai.TypeArray,
							Items: &genai.Schema{
								Type: genai.TypeObject,
								Properties: map[string]*genai.Schema{
									"row":  {Type: genai.TypeInteger},
									"col":  {Type: genai.TypeInteger},
									"text": {Type: genai.TypeString},
								},
								Required: []string{"row", "col", "text"},
							},
						},
						"bbox": {
							Type: genai.TypeObject,
							Properties: map[string]*genai.Schema{
								"x": {Type: genai.TypeNumber},
								"y": {Type: genai.TypeNumber},
								"w": {Type: genai.TypeNumber},
								"h": {Type: genai.TypeNumber},
							},
						},
						"low_confidence": {Type: genai.TypeBoolean},
					},
					Required: []string{"kind", "text"},
				},
			},
		},
		Required: []string{"confidence", "blocks"},
	}
}

// classifyPrompt asks for the coarse shape + confidence.
func classifyPrompt() string {
	return "You are an image content classifier. Look at the attached image and classify its dominant content into exactly one shape: " +
		"\"prose\" (dense readable text, like a document or article screenshot), " +
		"\"conversation\" (a chat/DM/thread screenshot with turns), " +
		"\"social_post\" (a single social-media post screenshot with author, body, engagement), " +
		"\"tabular\" (a table, spreadsheet, or chart), or " +
		"\"creative\" (a photo, illustration, or artwork with no dominant text). " +
		"Return ONLY JSON matching the schema. Set confidence to your 0..1 certainty in the shape."
}

// extractPrompt returns the shape-specific extraction instruction. Each shape
// names the block kinds the model should emit so the uniform schema carries
// shape-appropriate structure. All prompts ask for a normalized [0,1] bbox per
// block when the block localizes to a region, and an overall confidence.
func extractPrompt(shape Shape) string {
	const common = " Return ONLY JSON matching the schema. For each block, when it corresponds to a distinct region of the image, set bbox to its NORMALIZED [0,1] bounding box (x,y = top-left, w,h = size). Set the top-level confidence to your 0..1 certainty in the extraction. Transcribe visible text verbatim; do not invent content."
	switch shape {
	case ShapeProse:
		return "Extract the readable text from this image as ordered blocks. Use kind \"heading\" (with level 1..6) or \"paragraph\" or \"list_item\" (with level for nesting). Preserve reading order." + common
	case ShapeConversation:
		return "This is a conversation/chat screenshot. Extract each message as a block with kind \"turn\"; put the speaker/handle and the message text into text (prefix with the speaker when visible). Preserve chronological order." + common
	case ShapeSocialPost:
		return "This is a social-media post screenshot. Extract blocks with kinds \"author\" (handle/name), \"body\" (post text), and \"metric\" (one per visible engagement count, e.g. likes/reposts/replies — put the label and number in text)." + common
	case ShapeTabular:
		return "This is a table, spreadsheet, or chart. Emit a single block with kind \"table\" and populate cells with the grid (0-based row/col). If it is a chart rather than a grid, emit kind \"chart_summary\" with a text description of the axes and series. Screenshots of tables are lossy — set low_confidence true on the block." + common
	default: // ShapeCreative
		return "This is a photo, illustration, or artwork. Emit a block with kind \"caption\" describing the visible subject, and, if any text is legibly present (a sign, label, or watermark), a block with kind \"text\" transcribing it." + common
	}
}

// describePrompt asks for a long, factual, embeddable description.
func describePrompt() string {
	return "Write a detailed, factual description of this image suitable for search indexing. " +
		"Describe the subject, setting, notable objects, any legible text, colors, and composition in 2-5 sentences. " +
		"Be concrete and avoid speculation; do not add commentary or a preamble — output only the description."
}

// altTextPrompt asks for a single concise accessibility line under maxChars.
func altTextPrompt(maxChars int) string {
	return fmt.Sprintf("Write concise alternative text (alt text) for this image for screen-reader users. "+
		"One sentence, under %d characters, describing the essential content and function of the image. "+
		"Do not start with \"image of\" or \"picture of\"; output only the alt text with no quotes or preamble.", maxChars)
}
