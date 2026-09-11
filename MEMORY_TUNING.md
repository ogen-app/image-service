# Memory tuning (Railway)

How to set the runtime-memory knobs for a **lightly-loaded** `image-service` on
Railway where **memory consumption translates directly to budget**.

## Billing model that drives these choices

Railway bills **actual measured usage** (GB-hours of real RSS + vCPU-hours), not
the plan cap or any limit you configure. A lightly-loaded service is idle for the
vast majority of its hours, so the **idle baseline dominates the bill** — not the
rare burst peak. The goal is therefore to keep idle RSS low, not just to cap the
peak.

The service holds no state across calls (every buffer is request-local and
bounded by the worker semaphore + `IMAGE_MAX_PIXELS`), so the "leak" seen in
metrics is reclaimable memory the Go runtime / libvips / cgroup hasn't returned —
a flat plateau after a burst, not a rising staircase. The knobs below make RSS
track real usage.

## Recommended values

```bash
IMAGE_SERVICE_SCAVENGE_ON_IDLE=true     # the actual money-saver
IMAGE_SERVICE_GC_PERCENT=50             # keep default; minor lever here
IMAGE_SERVICE_MEMORY_LIMIT_RATIO=0.7    # safety net, tune to your cap (see below)
IMAGE_SERVICE_WORKERS=2                 # halves peak concurrent decode memory
```

### `SCAVENGE_ON_IDLE=true` — the one that saves money

Railway bills measured RSS over time, and the service is idle most hours. The idle
scavenge (`debug.FreeOSMemory` once the worker pool drains) is what drops the
burst plateau back toward baseline, so most billing samples catch the low idle
number. libvips decodes leave reclaimable native pages behind, so this matters
even more here than for a pure-Go service. Keep it on.

### `GC_PERCENT=50` — minor lever, don't overthink

GOGC only affects the Go heap high-water mark during activity. This service's Go
heap is modest (one decoded image buffer + gRPC buffers); the heavy allocation is
**inside libvips' native heap**, which GOGC doesn't touch. 50 gives slightly lower
burst peaks at trivial CPU cost. Don't go below ~40.

### `MEMORY_LIMIT_RATIO` — a safety net, not a cost lever

This sets `GOMEMLIMIT`, a **soft limit on Go-runtime-managed memory**. Key nuance:
**`GOMEMLIMIT` does not account for libvips' native (off-heap) allocations**,
which count against the container cgroup independently — so it is **not a
cgroup-wide OOM backstop**. A burst of large decodes can still exhaust the cgroup
regardless of `GOMEMLIMIT`. Leave room for them:

| Your Railway memory limit | Suggested ratio | Reasoning |
|---|---|---|
| ≤ 1 GB | **0.6–0.7** | reserve room for concurrent libvips decodes so a burst doesn't OOM |
| ≥ 4 GB (or unset) | 0.8–0.9 | native headroom is ample; ratio barely binds |

If you haven't set an explicit Railway memory limit, `memory.max` is likely your
plan's large default, so the derived `GOMEMLIMIT` does nothing. **Set an explicit
limit** — it costs nothing on usage-based billing, gives a hard backstop, and
makes the ratio meaningful.

## Two bigger levers

- **`IMAGE_SERVICE_WORKERS`** (default 4): the real driver of burst RSS is the
  number of images decoded concurrently, each holding a full pixel buffer bounded
  by `IMAGE_MAX_PIXELS`. Dropping to **2** halves the peak.
- **`IMAGE_MAX_PIXELS`** (default 100 MP): the per-image bomb guard is also the
  per-image memory ceiling. A 100 MP image at 4 bytes/pixel is ~400 MB decoded;
  lower it if your real inputs are smaller and you want a tighter peak.

## Note on the vision model

The classify/extract/describe/alt-text calls stream the (bounded, normalized)
image bytes to **Gemini over HTTPS**; the vision compute runs on Gemini's servers,
not here, so it adds little to this service's own RSS. The memory profile is
dominated by libvips decode/encode, exactly as pdf-service is dominated by
libpdfium.

## Verify after deploy

Run a couple of Extract/PrepareAttachment calls and confirm memory returns toward
baseline (reuse) rather than climbing across bursts (a real leak). At
`LOG_LEVEL=debug`, each scavenge logs `component=imgengine.scavenge`; boot logs
the derived `GOMEMLIMIT` under `component=runtimetune`.
