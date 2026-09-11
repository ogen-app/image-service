// Package logging gives image-service a single structured, leveled,
// context-aware logging foundation on stdlib log/slog — the gRPC counterpart of
// the Ogen API's logging unification (CON-107), identical to the pdf/audio
// siblings. New builds the process logger from config, installs it as the slog
// default, and routes the stdlib log package and gRPC's framework logger through
// it so stray or dependency logs stay structured. The ContextHandler
// auto-injects request_id/tenant_id, and the interceptors here supply those ids
// per RPC and emit one structured access-log line each.
package logging

import (
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/ogen-app/image-service/internal/config"
)

// New builds the logger from cfg, sets it as the slog default, and installs the
// stdlib->slog and gRPC->slog bridges. Call once at boot, right after config
// load, so early errors are structured. Returns the logger for explicit wiring
// (e.g. the interceptors); application code can also use the package-level slog
// functions.
func New(cfg *config.Config) *slog.Logger {
	logger := slog.New(ContextHandler{newBaseHandler(os.Stderr, cfg)})
	slog.SetDefault(logger)

	// Safety net: any stray stdlib log.Print* (ours or a dependency's that
	// survived the sweep) lands as a structured line instead of raw stderr.
	log.SetFlags(0)
	log.SetOutput(slog.NewLogLogger(logger.Handler(), slog.LevelInfo).Writer())

	// Route gRPC's own framework diagnostics through slog (the analog of injecting
	// the shared logger into a subsystem — River in CON-107 §5.1).
	installGRPCBridge(logger)

	return logger
}

// newBaseHandler selects the JSON (prod) or text (local) handler on w at the
// configured minimum level. Split out so tests can capture output.
func newBaseHandler(w io.Writer, cfg *config.Config) slog.Handler {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}
	if strings.EqualFold(strings.TrimSpace(cfg.LogFormat), "text") {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// parseLevel maps LOG_LEVEL to a slog.Level; unknown/empty falls back to info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
