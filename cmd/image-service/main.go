// Command image-service is the gRPC image-compute microservice (CON-281). It
// serves image.v1.ImageService (Extract / PrepareAttachment / GenerateAltText)
// backed by libvips (govips, CGO) and the Gemini vision model, plus a standard
// grpc.health.v1 endpoint. It is the SINGLE authority for all image compute in
// Ogen and is internal-only — the Ogen API reaches it over the Railway private
// network (plaintext h2c, no public port).
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	imagev1 "github.com/ogen-app/image-service/gen/image/v1"
	"github.com/ogen-app/image-service/internal/config"
	"github.com/ogen-app/image-service/internal/imgengine"
	"github.com/ogen-app/image-service/internal/logging"
	"github.com/ogen-app/image-service/internal/runtimetune"
	"github.com/ogen-app/image-service/internal/server"
	"github.com/ogen-app/image-service/internal/vision"
)

// serviceName is the health-check service key for ImageService (registered
// alongside the "" overall-server key so orchestrators can probe either).
const serviceName = "image.v1.ImageService"

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Pre-logger: config drives the logger's level/format, so a load failure can
		// only report through the stdlib default (CON-107 keeps boot log.Fatal*).
		log.Fatalf("image-service: config: %v", err)
	}

	logger := logging.New(cfg)

	// Bound the heap to the container and return burst-freed memory to the OS so
	// RSS tracks real usage instead of holding a high-water mark.
	runtimetune.Apply(logger, cfg)

	// libvips (CGO) is initialised inside the engine; New fails fast if it isn't
	// linkable so a misconfigured deploy surfaces at boot, not on the first request.
	engine, err := imgengine.New(imgengine.Config{
		MaxPixels:      cfg.MaxPixels,
		MaxUploadBytes: cfg.MaxUploadBytes,
		HTTPTimeout:    cfg.HTTPTimeout,
		Workers:        cfg.WorkerConcurrency,
		ScavengeOnIdle: cfg.ScavengeOnIdle,
	})
	if err != nil {
		logger.Error("init engine", "component", "boot", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			logger.Error("close engine", "component", "boot", "err", err)
		}
	}()

	// The vision client is key-optional: with no GEMINI_API_KEY, PrepareAttachment
	// (without alt text) still serves and any model-requiring RPC returns
	// Unavailable until a key is set.
	vis, err := vision.New(context.Background(), cfg.GeminiAPIKey)
	if err != nil {
		logger.Error("init vision", "component", "boot", "err", err)
		os.Exit(1)
	}
	if !vis.Available() {
		logger.Warn("no gemini api key configured; Extract/GenerateAltText will return Unavailable",
			"component", "boot")
	}

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", cfg.Port)
	if err != nil {
		logger.Error("listen", "component", "boot", "addr", cfg.Port, "err", err)
		os.Exit(1)
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor(logger)),
		grpc.ChainStreamInterceptor(logging.StreamServerInterceptor(logger)),
	)
	imagev1.RegisterImageServiceServer(srv, server.New(engine, vis, server.Defaults{
		ClassifyModel:       cfg.VisionClassifyModel,
		ExtractModel:        cfg.VisionExtractModel,
		EscalateModel:       cfg.VisionEscalateModel,
		AltTextModel:        cfg.VisionExtractModel, // alt text uses the extract-tier model by default
		AltTextMaxChars:     cfg.AltTextMaxChars,
		ConfidenceThreshold: cfg.ConfidenceThreshold,
	}))

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus(serviceName, healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	// Server reflection lets grpcurl and other tooling introspect the service over
	// the private network without a copy of the proto — handy for debugging an
	// internal-only service that has no public API surface.
	reflection.Register(srv)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down", "component", "boot")
		srv.GracefulStop() // drains in-flight RPCs
	}()

	logger.Info("listening", "component", "boot", "addr", cfg.Port, "workers", cfg.WorkerConcurrency)
	if err := srv.Serve(lis); err != nil {
		logger.Error("serve", "component", "boot", "err", err)
		os.Exit(1)
	}
}
