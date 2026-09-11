package logging

import (
	"context"
	"log/slog"
)

// ctxKey is an unexported context key type so correlation ids can't be forged or
// collided with by other packages (matches the Ogen API's tenantctx pattern,
// CON-107 §5.2).
type ctxKey int

const (
	requestIDKey ctxKey = iota
	tenantIDKey
)

// WithRequestID returns ctx carrying the correlation request id. The gRPC
// interceptor sets this from the inbound x-request-id (or a fresh id); every slog
// line made with that ctx then carries request_id automatically.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithTenantID returns ctx carrying the tenant id (set from x-tenant-id when the
// caller supplies it).
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantIDKey, id)
}

// RequestID reports the request id carried by ctx, if any (and non-empty).
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok && id != ""
}

// TenantID reports the tenant id carried by ctx, if any (and non-empty).
func TenantID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(tenantIDKey).(string)
	return id, ok && id != ""
}

// ContextHandler decorates a slog.Handler, enriching every record with the
// request_id and tenant_id carried by the context (omitted when absent — no empty
// keys). This keeps call sites clean: slog.InfoContext(ctx, "msg", ...)
// auto-enriches. Mirrors the Ogen API's logging.ContextHandler (CON-107 §5.1);
// designed so trace ids (CON-96) can be added here later without touching call
// sites.
type ContextHandler struct {
	slog.Handler
}

// Handle appends the context correlation ids, then delegates.
func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := RequestID(ctx); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	if id, ok := TenantID(ctx); ok {
		r.AddAttrs(slog.String("tenant_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup re-wrap so the enrichment survives derived loggers
// (embedding alone would return the bare inner handler).
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{h.Handler.WithAttrs(attrs)}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{h.Handler.WithGroup(name)}
}
