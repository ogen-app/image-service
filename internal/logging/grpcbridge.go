package logging

import (
	"fmt"
	"log/slog"
	"os"

	"google.golang.org/grpc/grpclog"
)

// installGRPCBridge routes gRPC's framework logger through slog so connection and
// transport diagnostics are structured (component=grpc.internal). This is the
// image-service analog of CON-107's "inject the shared logger into the subsystem"
// (River there, gRPC here).
//
// gRPC's framework "info" is verbose (per-connection lifecycle), so it is mapped
// to slog debug — hidden at the default info level but surfaced under
// LOG_LEVEL=debug, matching gRPC's own quiet-by-default behaviour. Warnings and
// errors map straight through.
func installGRPCBridge(logger *slog.Logger) {
	grpclog.SetLoggerV2(&grpcBridge{l: logger.With("component", "grpc.internal")})
}

type grpcBridge struct{ l *slog.Logger }

func (b *grpcBridge) Info(args ...any)               { b.l.Debug(fmt.Sprint(args...)) }
func (b *grpcBridge) Infoln(args ...any)             { b.l.Debug(fmt.Sprint(args...)) }
func (b *grpcBridge) Infof(f string, args ...any)    { b.l.Debug(fmt.Sprintf(f, args...)) }
func (b *grpcBridge) Warning(args ...any)            { b.l.Warn(fmt.Sprint(args...)) }
func (b *grpcBridge) Warningln(args ...any)          { b.l.Warn(fmt.Sprint(args...)) }
func (b *grpcBridge) Warningf(f string, args ...any) { b.l.Warn(fmt.Sprintf(f, args...)) }
func (b *grpcBridge) Error(args ...any)              { b.l.Error(fmt.Sprint(args...)) }
func (b *grpcBridge) Errorln(args ...any)            { b.l.Error(fmt.Sprint(args...)) }
func (b *grpcBridge) Errorf(f string, args ...any)   { b.l.Error(fmt.Sprintf(f, args...)) }

// Fatal* keep grpclog's exit semantics: log at error, then exit non-zero.
func (b *grpcBridge) Fatal(args ...any)            { b.l.Error(fmt.Sprint(args...)); os.Exit(1) }
func (b *grpcBridge) Fatalln(args ...any)          { b.l.Error(fmt.Sprint(args...)); os.Exit(1) }
func (b *grpcBridge) Fatalf(f string, args ...any) { b.l.Error(fmt.Sprintf(f, args...)); os.Exit(1) }

// V reports whether a log at the given verbosity level is enabled. gRPC's default
// verbosity is 0, so only report level 0 as enabled to avoid spamming the slog
// handler with gRPC's most verbose traces.
func (b *grpcBridge) V(l int) bool { return l <= 0 }
