# image-service

gRPC image-compute microservice for Ogen (CON-281). It is the **single authority
for all image compute**: the Ogen API hands it short-lived presigned URLs, and it
validates, normalizes, and runs the Gemini vision model over images — returning
structured extraction, a description, alt text, and per-call token usage.

It is the third asset-ingestion service beside
[pdf-service](https://github.com/ogen-app/pdf-service) (CON-103) and
[document-service](https://github.com/ogen-app/document-service) (CON-280), and
the media sibling of [audio-service](https://github.com/ogen-app/audio-service)
(CON-282). Together CON-280/281/282 complete the asset-ingestion trio.

Stateless and **internal-only** — the API reaches it over the Railway private
network (plaintext h2c, no public port). It holds no state and no database: ogen
owns all persistence, metering, embedding, quota, and the HTTP surface.

## Why a separate service (CGO + libvips)

Decoding HEIC/AVIF/TIFF and doing a bomb-safe normalize needs a native image
library — **libvips** (via [govips](https://github.com/davidbyttow/govips), CGO).
Keeping that out of the Ogen API lets the API stay a `CGO_ENABLED=0` static
binary; this service is the CGO deployable, exactly like pdf-service isolates
`libpdfium`.

## Locked decisions (D1–D6)

- **D1 — FULL AI service.** The vision model runs **in this service**, not in
  ogen. The service normalizes (libvips) *and* runs classify → extract →
  describe → alt text, returning Blocks + description + alt text + per-call token
  usage. (Diverges from audio-service's normalize-only split, on purpose.) ogen
  persists / prices / embeds and owns the API surface + quota gate.
- **D2 — Gemini vision via the Gemini Developer API.** Reuse the existing
  `GEMINI_API_KEY` + `google.golang.org/genai` (same path as the ogen embedder
  and audio-service). No Vertex, no GCP service account in v1. **EU data residency
  is DEFERRED** to a future Vertex `europe-west` endpoint swap (documented below;
  shared with audio-service and CON-105). Model ids come from config/request,
  never compiled in.
- **D3 — Defer raw-image embeddings.** v1 is searchable via the **description +
  extracted text** (ogen's existing text embedder → `assets_chunks` type IMG).
  Raw visual vectors are deferred (uncertain ROI; would need a Vertex-EU
  image-embed endpoint). Mirrors audio.
- **D4 — Ship generated alt text by default, protect user edits.** ogen holds an
  `alt_text_edited_by_user` flag (on both `assets` and `post_attachments`) and
  auto-fills AltText only when empty/unedited; re-extraction never overwrites a
  user edit. `GenerateAltText` is separately invocable for on-demand regeneration
  (CON-122).
- **D5 — Escalate once on low confidence.** When the classify OR the per-shape
  extract confidence is below the threshold, that step re-runs **exactly once** on
  a stronger escalate model. The outcome (`escalated`, `escalation_improved`) is
  recorded regardless of whether it helped.
- **D6 — image-service is the SINGLE image ingress.** ogen's pure-Go
  `src/storage/imageprobe` is **deleted**; BOTH content-bank IMG assets AND
  post-attachment images route through this service. All image *compute* moves
  here (sniff / validate / dims / animated / checksum / EXIF-strip / normalize /
  alt-text / extraction); ogen keeps only thin persistence, the HTTP endpoints,
  the River job, and the CON-122 Zernio publish read. **This makes image-service a
  HARD dependency for image ingestion**: an empty/unreachable `IMAGE_SERVICE_ADDR`
  fails image uploads (no `imageprobe` fallback). Net-new privacy fix:
  **EXIF/geolocation is now stripped on every image**, so it no longer leaks to
  Zernio on publish.

## The two ingress flows

| Flow | RPC | Path |
|------|-----|------|
| **Content bank** (full) | `Extract` | fetch → validate + **header-first bomb guard** → normalize (re-encode PNG/JPEG, strip EXIF/geo, keep full resolution for text-bearing shapes, downscale non-text) → PUT derivative → **classify (low-res) → extract (high-res, per-shape) → describe → alt text**, escalate once → return Blocks + description + alt text + normalized meta + token usage |
| **Post attachment** (light) | `PrepareAttachment` | fetch → validate + bomb guard → metadata (mime/dims/animated/frames/checksum) → **EXIF/geo strip with PIXELS PRESERVED** (no resize, lossless re-emit) → PUT derivative → optional alt text. **No vision extraction.** |

`GenerateAltText(source_url, max_chars, model)` re-generates alt text for an
existing image on demand.

All three RPCs are **unary** and presigned-URL based (source GET, dest PUT). The
service fetches the original bytes, holds them once in memory (bounded by
`IMAGE_MAX_UPLOAD_BYTES` and `IMAGE_MAX_PIXELS`), and PUTs the derivative.

### Contract

`image.v1.ImageService` lives in the **shared** `buf.build/ogen-app/proto`
module (**v1.3.0**, CON-220) — NOT in this repo. This service is a *consumer* of
that contract, exactly like ogen / pdf-service / audio-service: `make proto`
(`buf generate buf.build/ogen-app/proto:v1.3.0`) pulls it and generates the Go
stubs under [`gen/`](gen/) (committed; see that dir's README).

`image.v1` **reuses the shared `documents.v1.Anchor`** — the one citation shape
across every asset kind (CON-280/281). `image.v1.Block.anchor` is typed as
`documents.v1.Anchor`, and an image block sets
`kind = ANCHOR_KIND_IMAGE_REGION` with `bbox` = the normalized `[0,1]` region.
Because of that import, `buf generate` emits both `gen/image/v1/` and
`gen/documents/v1/`.

> **`make proto` needs the tag published first (CON-220):**
> `buf.build/ogen-app/proto:v1.3.0` — which carries `image.v1` plus the extended
> `documents.v1.Anchor` (`ANCHOR_KIND_IMAGE_REGION` + `Bbox`) — is **not yet
> published to the BSR** (those proto-repo changes are still uncommitted). Until
> the tag exists, `make proto` cannot resolve the module and the stubs cannot be
> regenerated.

### Shapes

The classify pass routes one of five shapes, each with a shape-specific
extraction prompt:

| Shape | Block kinds emitted |
|-------|---------------------|
| `prose` | `heading` (level 1..6) / `paragraph` / `list_item` |
| `conversation` | `turn` (chronological) |
| `social_post` | `author` / `body` / `metric` |
| `tabular` | `table` (with `cells` grid) or `chart_summary` — **always low-confidence** (screenshots of tables are lossy; ogen surfaces a "supply the source file" hint) |
| `creative` | `caption` (+ `text` when legible text is present) |

### Formats

Accepted: **JPEG, PNG, WebP, HEIC, AVIF, GIF (first frame), TIFF, BMP**.
**SVG / vector is rejected terminally** — a vector has no honest pixel dimensions
to bomb-guard and libvips would rasterize it at an attacker-chosen scale.

### Safety

- **Header-first decompression-bomb guard.** Dimensions are read from the header
  and rejected against `IMAGE_MAX_PIXELS` **before any pixels are decoded**, so a
  `42000x42000` PNG never allocates tens of GB. A second guard bounds the fetched
  encoded byte size (`IMAGE_MAX_UPLOAD_BYTES`).
- **EXIF/geolocation stripped everywhere** (D6 privacy fix).
- **Bounded memory** via the worker semaphore + the pixel/byte caps.
- Presigned URL query strings (credentials) are **redacted** from every error
  message; the full URL appears only in server-side logs.

### Metering

The service **returns per-call `TokenUsage`** (model, step, input, output). ogen's
Recorder **prices** it via its existing **`gemini` vendor** (CON-86,
`Operation:"vision_extract"`) and snapshots it. The quota gate lives in ogen,
*before* the model call (CON-208). Nothing is priced in this service.

### Error semantics

| Condition | gRPC code | API behaviour |
|-----------|-----------|---------------|
| SVG/vector, corrupt/undecodable, unsupported format, oversize (pixels or bytes) | `InvalidArgument` | reject upload (terminal, no retry) |
| Presigned GET/PUT transfer failure | `Internal` | retry the whole step |
| Gemini 5xx / unavailable | `Unavailable` | retry |
| Gemini 429 rate-limited | `ResourceExhausted` | back off, retry |
| Gemini / engine deadline | `DeadlineExceeded` | retry |
| No `GEMINI_API_KEY` (any vision RPC) | `Unavailable` | key can be set without a restart |
| Empty `source_url` | `InvalidArgument` | — |

In `Extract`, a failure in a **late** step (describe/alt-text) does not fail the
call: the per-step `*_ok` flags let ogen persist the partial result and keep the
spend already incurred.

Also serves standard `grpc.health.v1.Health` (both the `""` overall key and
`image.v1.ImageService`) for orchestrator probes.

## EU data residency (deferred)

v1 uses the **Gemini Developer API** for simplicity (D2). A later revision can
switch the vision client to **Vertex AI in `europe-west`** without changing the
RPC contract — the `genai` SDK supports both backends. In `internal/vision`,
swap `Backend: genai.BackendGeminiAPI` for `genai.BackendVertexAI` + a location;
no other code changes. Shared with audio-service and CON-105.

## Configuration

Environment variables (no prefix, matching the Ogen API's style):

| Var | Default | Purpose |
|-----|---------|---------|
| `IMAGE_SERVICE_LISTEN` | `:50051` | gRPC listen address (bare port accepted) |
| `GEMINI_API_KEY` | (unset) | Gemini Developer API key; empty ⇒ vision RPCs return `Unavailable` |
| `VISION_CLASSIFY_MODEL` | `gemini-2.5-flash` | **default** classify model; request wins |
| `VISION_EXTRACT_MODEL` | `gemini-2.5-pro` | **default** extract/describe/alt model; request wins |
| `VISION_ESCALATE_MODEL` | `gemini-2.5-pro` | **default** one-shot escalation model; request wins |
| `ALT_TEXT_MAX_CHARS` | `420` | cap on generated alt text when request leaves it 0 |
| `IMAGE_MAX_PIXELS` | `100000000` | header-first bomb guard (also the per-image memory ceiling) |
| `IMAGE_MAX_UPLOAD_BYTES` | `67108864` | encoded-byte bomb guard (64 MiB) |
| `CONFIDENCE_THRESHOLD` | `0.6` | escalate when a step's confidence is below this; request wins |
| `IMAGE_SERVICE_WORKERS` | `4` | max concurrent decode/encode operations |
| `IMAGE_HTTP_TIMEOUT` | `60s` | per presigned GET/PUT round-trip |
| `IMAGE_SERVICE_GC_PERCENT` | `50` | GC target (GOGC); `<=0` keeps the runtime default |
| `IMAGE_SERVICE_MEMORY_LIMIT_RATIO` | `0.8` | soft mem limit (GOMEMLIMIT) as a fraction of the cgroup limit; ignored if `GOMEMLIMIT` is set or no cgroup limit is found |
| `IMAGE_SERVICE_SCAVENGE_ON_IDLE` | `true` | return freed memory to the OS once the worker pool drains |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `LOG_FORMAT` | `json` | `json` (prod) or `text` (local) |

See [MEMORY_TUNING.md](MEMORY_TUNING.md) for the Railway memory-cost knobs.

## Build & run

The engine links **libvips** via CGO, so `CGO_ENABLED=1` and `libvips-dev` are
required to build. On macOS: `brew install vips`. The Docker runtime image is
**debian-slim** (NOT scratch — libvips is a shared library with a large codec
graph) with the libvips runtime package + `grpc_health_probe`.

First-time setup in a fresh checkout: generate the committed artifacts that can't
be produced offline —

```sh
make proto                       # buf generate from the pinned shared module → gen/image/v1 + gen/documents/v1 (needs buf; the tag must be published)
go mod tidy                      # resolve deps and write go.sum (the Dockerfile COPYs it)
```

then build and test:

```sh
CGO_ENABLED=1 go build ./cmd/image-service
CGO_ENABLED=1 go test -race ./...

GEMINI_API_KEY=... go test -tags=eval ./eval/...   # golden eval gate (see eval/README.md)

docker build -t image-service .
```

The API reaches it at `image-service:50051` in compose / `image-service.railway.internal:50051`
in prod, via an `IMAGE_SERVICE_ADDR` on the API side.

## Layout

```
gen/                         # committed stubs generated from buf.build/ogen-app/proto (image/v1 + documents/v1; see its README)
cmd/image-service/main.go    # grpc.NewServer + health + reflection wiring, graceful shutdown
internal/config/             # envconfig
internal/server/             # the three RPCs + error mapping (imgengine + vision orchestration)
internal/imgengine/          # libvips: decode/validate/bomb-guard/orient/EXIF-strip/normalize + presigned transfer
internal/vision/             # Gemini vision: classify/extract/describe/alt + one-shot escalation + token usage
internal/logging/            # slog + x-request-id/x-tenant-id interceptors (CON-107)
internal/runtimetune/        # GOGC + GOMEMLIMIT-from-cgroup
eval/                        # golden eval set + harness (model/prompt regression gate)
```
