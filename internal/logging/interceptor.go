package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	// RequestIDHeader is the gRPC metadata key the calling API sets so an image
	// request correlates back to the originating HTTP request / job (CON-107
	// request->job correlation, extended across the service boundary). Honored
	// inbound; a fresh id is generated when absent. MUST match the imageclient
	// header name in the Ogen API.
	RequestIDHeader = "x-request-id"
	// TenantIDHeader optionally carries the tenant so image logs join the API's by
	// tenant_id.
	TenantIDHeader = "x-tenant-id"
)

// StreamServerInterceptor enriches every streaming RPC's context with a
// request_id (inbound x-request-id or a fresh one) and optional tenant_id, echoes
// the request_id back to the caller as a response header, and emits exactly one
// structured access-log line on completion. gRPC analog of CON-107's Fiber
// requestid + structured access-log middleware (FR5, FR6, FR7). ImageService is
// all-unary today; this exists so a future streaming RPC is covered uniformly.
func StreamServerInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, reqID := enrich(ss.Context())
		_ = ss.SetHeader(metadata.Pairs(RequestIDHeader, reqID))

		start := time.Now()
		err := handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
		logRPC(ctx, logger, info.FullMethod, err, start)
		return err
	}
}

// UnaryServerInterceptor is the unary counterpart (used by every ImageService RPC
// and the health endpoint).
func UnaryServerInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, reqID := enrich(ctx)
		_ = grpc.SetHeader(ctx, metadata.Pairs(RequestIDHeader, reqID))

		start := time.Now()
		resp, err := handler(ctx, req)
		logRPC(ctx, logger, info.FullMethod, err, start)
		return resp, err
	}
}

// enrich attaches the correlation ids to ctx and returns the resolved request id.
func enrich(ctx context.Context) (context.Context, string) {
	reqID := metadataValue(ctx, RequestIDHeader)
	if reqID == "" {
		reqID = newRequestID()
	}
	ctx = WithRequestID(ctx, reqID)
	if tid := metadataValue(ctx, TenantIDHeader); tid != "" {
		ctx = WithTenantID(ctx, tid)
	}
	return ctx, reqID
}

func metadataValue(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(key); len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

// newRequestID returns a random 128-bit hex id (crypto/rand never realistically
// fails; degrade to a constant marker rather than panic if it does).
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// logRPC emits the single access-log line. Server-fault codes log at error,
// client-fault at warn, success at info; health probes drop to debug so they
// don't spam the log (CON-107 FR6/FR7).
func logRPC(ctx context.Context, logger *slog.Logger, method string, err error, start time.Time) {
	code := status.Code(err)
	attrs := []any{
		"component", "grpc",
		"method", method,
		"code", code.String(),
		"latency_ms", time.Since(start).Milliseconds(),
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		attrs = append(attrs, "peer", p.Addr.String())
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	logger.Log(ctx, levelForCode(method, code), "rpc", attrs...)
}

func levelForCode(method string, code codes.Code) slog.Level {
	if isHealthMethod(method) {
		return slog.LevelDebug // health probes: quiet unless debugging
	}
	switch code {
	case codes.OK:
		return slog.LevelInfo
	case codes.Internal, codes.Unknown, codes.Unavailable, codes.DataLoss:
		return slog.LevelError // server-fault (CON-107 FR7: 5xx at error)
	default:
		return slog.LevelWarn // client-fault (InvalidArgument, etc.): not an error
	}
}

func isHealthMethod(method string) bool {
	return strings.HasPrefix(method, "/grpc.health.v1.Health/")
}

// wrappedStream lets the interceptor swap in an enriched context that reaches the
// handler via stream.Context() (a ServerStream's context is otherwise
// read-only).
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
