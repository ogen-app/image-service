// Package config loads image-service settings from the environment via envconfig.
package config

import (
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config holds the service settings. Read from the environment with no global
// prefix (each field names its own var), matching the Ogen API's and sibling
// services' config style (CON-107).
type Config struct {
	// Port is the gRPC listen address. Accepts a full "host:port" or a bare port
	// (e.g. "50051", as Railway's PORT provides) — Load normalises a bare port to
	// ":port" so net.Listen gets the leading colon it requires. Default :50051
	// matches pdf/audio/document-service.
	Port string `envconfig:"IMAGE_SERVICE_LISTEN" default:":50051"`

	// GeminiAPIKey authenticates the vision model calls against the Gemini
	// Developer API — the same key the Ogen embedder and audio-service use
	// (decision D2: Gemini Developer API, no Vertex/GCP service account in v1).
	// Empty is tolerated at boot so PrepareAttachment's metadata path still
	// serves; any RPC that needs the model then returns Unavailable.
	GeminiAPIKey string `envconfig:"GEMINI_API_KEY" default:""`

	// VisionClassifyModel is the DEFAULT Gemini model id for the low-res classify
	// pass. VisionExtractModel is the DEFAULT for the high-res extract/describe
	// pass. VisionEscalateModel is the DEFAULT for the one-shot escalation. All
	// three are only fallbacks: each is a per-request field on ExtractRequest and
	// is NEVER compiled in (decision — model ids come from the request so ogen can
	// swap models without redeploying the service). The classify pass uses a
	// cheap/fast model; extract/escalate use a stronger one.
	VisionClassifyModel string `envconfig:"VISION_CLASSIFY_MODEL" default:"gemini-2.5-flash"`
	VisionExtractModel  string `envconfig:"VISION_EXTRACT_MODEL"  default:"gemini-2.5-pro"`
	VisionEscalateModel string `envconfig:"VISION_ESCALATE_MODEL" default:"gemini-2.5-pro"`

	// AltTextMaxChars caps generated alt text length when a request leaves
	// alt_text_max_chars at 0. Alt text is a short accessibility string, not a
	// caption; keep it terse. ~420 chars mirrors common platform alt-text limits.
	AltTextMaxChars int `envconfig:"ALT_TEXT_MAX_CHARS" default:"420"`

	// MaxPixels is the header-first decompression-bomb guard: an image whose
	// width*height exceeds this is rejected BEFORE any pixels are decoded (the
	// dimensions are read from the header). 100 MP (e.g. 10000x10000) is generous
	// for real photos while stopping a "42000x42000 PNG" bomb that would allocate
	// tens of GB on decode. Terminal (InvalidArgument).
	MaxPixels int64 `envconfig:"IMAGE_MAX_PIXELS" default:"100000000"`

	// MaxUploadBytes bounds the encoded byte size fetched from the presigned GET
	// URL, a second bomb guard independent of pixel area (a small-dimension image
	// can still be a huge encoded payload). Terminal (InvalidArgument) when
	// exceeded. 64 MiB comfortably covers legitimate high-res photos.
	MaxUploadBytes int64 `envconfig:"IMAGE_MAX_UPLOAD_BYTES" default:"67108864"`

	// ConfidenceThreshold gates the one-shot escalation: when the classify OR
	// per-shape extract confidence is below this, that step re-runs once on
	// VisionEscalateModel. A per-request confidence_threshold (when > 0) overrides
	// this. 0.6 is a middle ground — escalate when the model is genuinely unsure
	// without paying for a second pass on every clear image.
	ConfidenceThreshold float64 `envconfig:"CONFIDENCE_THRESHOLD" default:"0.6"`

	// WorkerConcurrency bounds how many in-flight image operations run at once.
	// libvips decode is memory-heavy (bounded by MaxPixels but still large for a
	// 100 MP image), so this caps peak RSS the same way audio-service's Workers
	// caps concurrent ffmpeg children. Each Extract/PrepareAttachment holds one
	// slot for its decode+encode.
	WorkerConcurrency int `envconfig:"IMAGE_SERVICE_WORKERS" default:"4"`

	// HTTPTimeout bounds a single presigned fetch/PUT round-trip (source GET or
	// derivative PUT). A stalled origin is aborted rather than pinning a worker
	// slot; the API also applies its own per-call deadline on top.
	HTTPTimeout time.Duration `envconfig:"IMAGE_HTTP_TIMEOUT" default:"60s"`

	// GCPercent sets the GC target (Go's GOGC) via debug.SetGCPercent at boot. A
	// lower value collects more often, trading CPU for a smaller heap high-water
	// mark — worthwhile here since the service sits CPU-idle between bursts. <=0
	// leaves the runtime default (100) in place.
	GCPercent int `envconfig:"IMAGE_SERVICE_GC_PERCENT" default:"50"`

	// MemoryLimitRatio sets Go's soft memory limit (GOMEMLIMIT) to this fraction
	// of the container's cgroup memory limit, read at boot. It makes the GC lean
	// harder as the heap nears the cap, keeping RSS down and guarding against OOM
	// under a burst. Ignored when GOMEMLIMIT is set explicitly, when <=0, or when
	// no finite cgroup limit is found (e.g. local dev). libvips also allocates
	// native (off-heap) memory GOMEMLIMIT doesn't govern, so leave headroom.
	MemoryLimitRatio float64 `envconfig:"IMAGE_SERVICE_MEMORY_LIMIT_RATIO" default:"0.8"`

	// ScavengeOnIdle returns freed memory to the OS (debug.FreeOSMemory) once the
	// worker pool drains to idle after a burst, so container RSS tracks real usage
	// instead of holding a high-water mark. Runs off the request path.
	ScavengeOnIdle bool `envconfig:"IMAGE_SERVICE_SCAVENGE_ON_IDLE" default:"true"`

	// LogLevel is the minimum slog level: debug|info|warn|error. Unknown/empty
	// falls back to info. Bare LOG_LEVEL (not prefixed) matches the Ogen API's
	// knob so operators use identical settings across services (CON-107).
	LogLevel string `envconfig:"LOG_LEVEL" default:"info"`
	// LogFormat selects the slog handler: json (default, prod) or text (local).
	LogFormat string `envconfig:"LOG_FORMAT" default:"json"`
}

// Load reads and validates the configuration from the environment.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, err
	}
	// A bare port like "50051" is a valid env value (Railway's PORT) but net.Listen
	// needs "host:port" — without a colon it errors "missing port in address".
	// Prefix it so the service binds all interfaces on that port.
	if c.Port != "" && !strings.Contains(c.Port, ":") {
		c.Port = ":" + c.Port
	}
	return &c, nil
}
