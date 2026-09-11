# Generated gRPC stubs (do not edit by hand)

This directory holds the Go code generated from the **shared**
`buf.build/ogen-app/proto` module (CON-220) — NOT from a self-contained copy in
this repo. `image.v1.Block.anchor` is a `documents.v1.Anchor`, so `buf generate`
pulls both packages and the stubs land under two subdirectories:

- `gen/image/v1/` — `image.v1`: `ImageService{Server,Client}`,
  `ExtractRequest`/`Response`, `PrepareAttachmentRequest`/`Response`,
  `GenerateAltTextRequest`/`Response`, `Block`, `Cell`, `TokenUsage`,
  `NormalizedMeta`, and the `Shape` enum.
- `gen/documents/v1/` — `documents.v1`: the SHARED `Anchor` (with
  `ANCHOR_KIND_IMAGE_REGION`), `Bbox`, `AnchorKind`, and the rest of the
  documents contract that image.v1 imports.

Managed mode's `go_package_prefix` override keeps every import path under
`github.com/ogen-app/image-service/gen/...`.

## Regenerating

```sh
make proto        # == buf generate buf.build/ogen-app/proto:v1.3.0
```

The stubs are produced by `buf generate` against the **pinned** shared module —
they cannot be produced offline, so they are committed the same way the
pdf/audio/document-service siblings commit theirs. Regenerate with `make proto`
in a checkout with `buf` available, then commit the result.

> **Not published yet:** `make proto` requires
> `buf.build/ogen-app/proto:v1.3.0` (which carries `image.v1` + the extended
> `documents.v1.Anchor`) to be tagged/published to the BSR first. Until then the
> stubs cannot be generated.
