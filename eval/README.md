# Golden eval set (model/prompt regression gate)

The vision pipeline (classify → extract → describe → alt text) is driven by
prompts and a model id — both of which can silently regress. This eval set is the
**gate for model/prompt changes** (a CON-281 NFR): any change to
`internal/vision` prompts/schemas, or a bump of the default model, must keep these
fixtures passing.

It runs against the **real Gemini model** (there is no faithful offline stand-in
for a vision model), so it is behind the `eval` build tag and needs a
`GEMINI_API_KEY`. When the key is absent the harness **skips** rather than fails —
so it is advisory on forks and enforced on the canonical repo (see `ci.yml`, the
`eval` job).

```sh
GEMINI_API_KEY=... go test -tags=eval ./eval/...
```

## Layout

```
eval/
  README.md
  harness_test.go        # loads fixtures, runs the pipeline, asserts expectations
  fixtures/
    manifest.json        # one entry per fixture: file + expected shape/blocks/alt
    <name>.png|jpg|...   # the image bytes (committed; keep them small)
```

`fixtures/manifest.json` is a JSON array of cases:

```json
[
  {
    "file": "invoice.png",
    "expected_shape": "tabular",
    "min_blocks": 1,
    "must_contain_text": ["Total", "Invoice"],
    "expect_low_confidence": true,
    "alt_text_max_chars": 200
  },
  {
    "file": "chat.png",
    "expected_shape": "conversation",
    "min_blocks": 2,
    "must_contain_text": ["Alice", "Bob"]
  },
  {
    "file": "beach.jpg",
    "expected_shape": "creative",
    "alt_not_empty": true
  }
]
```

## What it asserts

For each fixture the harness runs the real pipeline over the local image bytes
and checks:

- **Classification** — the classified `Shape` equals `expected_shape`.
- **Extraction** — at least `min_blocks` blocks came back, and every string in
  `must_contain_text` appears (case-insensitively) somewhere in the concatenated
  block text. `expect_low_confidence` asserts the tabular low-confidence marker.
- **Alt text** — non-empty when `alt_not_empty`, and within
  `alt_text_max_chars` when set.

## Curating fixtures

Keep the set **small, diverse, and deterministic**: one clear example per shape
(prose / conversation / social_post / tabular / creative), plus the tricky cases
that motivated a prompt change (add a fixture with every regression). Prefer
synthetic/self-authored images to avoid licensing issues, and keep them under a
few hundred KB so the repo stays lean. Because the model is nondeterministic,
assertions are **thresholds and substrings**, never exact-match on generated
prose.
