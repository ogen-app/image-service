# Shared gRPC contract (CON-220). image.v1 lives in buf.build/ogen-app/proto,
# NOT in this repo; it imports the shared documents.v1 Anchor
# (ANCHOR_KIND_IMAGE_REGION + bbox). `make proto` generates the server stubs
# from a PINNED version of that module and commits gen/ so `go build` needs no
# buf. Because image.v1.Block.anchor is a documents.v1.Anchor, `buf generate`
# emits BOTH gen/image/v1/ and gen/documents/v1/. Bump PROTO_VERSION to adopt a
# new contract, then `make proto` and commit gen/.
#
# NOTE: image.v1 is not published yet — `make proto` requires
# buf.build/ogen-app/proto:v1.3.0 to be tagged/published first.
#
# The engine links libvips via CGO; set PKG_CONFIG_PATH so pkg-config finds
# vips.pc on non-standard installs. On macOS with Homebrew: `brew install vips`.
PROTO_MODULE  = buf.build/ogen-app/proto
PROTO_VERSION = v1.3.0

.PHONY: proto lint build test

proto:
	buf generate $(PROTO_MODULE):$(PROTO_VERSION)

lint:
	buf lint $(PROTO_MODULE):$(PROTO_VERSION)

build:
	CGO_ENABLED=1 go build ./...

test:
	CGO_ENABLED=1 go test -race ./...
