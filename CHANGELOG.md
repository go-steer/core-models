# Changelog

## Unreleased

## v0.5.0 (2026-10-09)

### Documentation

- ***Tested models* now shows three-run spreads.** GLM 5.2 holds parity with
  Claude across all three runs. Kimi K2 Thinking is corrected from "at
  parity" to close, below parity, with a mean intent of 0.941.
- **Breadth is now two columns:** tool calls and distinct tools per
  incident. The single published column counted repeats as tools.
- **New self-hosted section:**
  - gpt-oss-120b on vLLM.
  - Gemma 4 on vLLM, with thinking off (it looped on 7 of 31 incidents)
    and on.
  - The same Gemma 4 weights on Vertex AI. They scored the same, but vLLM
    let the model repeat tools far more often, which is still unexplained.

### Added

- **Declared rates:** `models[].rates` in profiles, in USD per million
  tokens (input, output, cached input, cache write), read with
  `Profile.RatesFor(id)`. They're for a self-hosted server no published
  catalog prices.
  - Validation refuses negative rates, and refuses rates with no input or
    output price, since a free model makes a cost ceiling that never trips.
- **Conformance corpus:** Gemma 4 on vLLM and on Vertex AI, nine servers in
  all. A corpus `meta.json` may carry the `extra_body` its recording was
  made with.

## v0.4.0 (2026-10-09)

### Added

- **`extra_body` in profiles, per profile and per model.** These are vendor
  request fields the dialect doesn't model, merged into each request, such
  as `chat_template_kwargs: {enable_thinking: true}` for Gemma 4 on vLLM.
  A field the adapter sets itself is refused when the profile is validated,
  and again at request time. The example profile in `deploy/gke-vllm`
  enables Gemma 4's thinking, without which it looped on identical tool
  calls (go-steer/mast#514).

- **vLLM cache writes:** `created_cache_tokens`, the prompt tokens vLLM
  wrote to its prefix cache (reported with
  `--enable-prompt-tokens-details`), is recorded as
  `usage.Detail.CacheWriteTokens`.

## v0.3.0 (2026-10-09)

Self-hosting: the GKE vLLM fixture and a header timeout for servers that
never answer.

### Added

- **`deploy/gke-vllm`:** a fixture for vLLM on one GPU in GKE, published
  over Private Service Connect.
  - Model overlays for gpt-oss-120b, Qwen3-Coder-30B and Gemma 4 26B-A4B.
  - `psc.sh` sets up the producer subnet, the ServiceAttachment and the
    consumer endpoint.
  - A matching profile example.
  - It encodes the lessons from bringing it up: egress, PVC sizing,
    `enableServiceLinks`, the API-key newline, gpt-oss's `auto`-only tool
    choice, and `--enable-prompt-tokens-details`.
  - Documented on the new *Self-hosting on GKE* page.
- **Conformance corpus:** a recording from that deployment
  (`vllm-0.31-gpt-oss-120b`).
- **Live smoke:** `CORE_MODELS_LIVE_PROFILE_FILE` declares profiles for a
  server with its own URL and credential.

### Fixed

- **A server that never answers no longer holds the caller forever.**
  `retry.Policy.HeaderTimeout` (default five minutes) abandons an attempt
  with no response headers and retries it like a dropped connection. The
  final failure is `retry.ErrNoResponse`. The limit never applies to a
  response body, so a long stream is unaffected. Found when a Vertex AI
  request waited 28 minutes for headers and froze a judged eval run.

### Documentation

- **New *Tested models* page.** It explains what the conformance and
  tool-calling parity tests measure, and publishes the 2026-10-09 parity
  run against Claude: GLM 5.2 and Kimi K2 Thinking at parity on intent, and
  zero malformed calls in 858. Llama 4 Maverick on Vertex AI is documented
  as unsupported for tool use, with the reason.

## v0.2.0 (2026-10-09)

Fixes found by live runs against five Vertex AI partner models, before the
tool-calling parity run.

### Added

- **Per-model capabilities:** `models[].capabilities` overrides a profile's
  capabilities for one model, via `Profile.CapabilitiesFor(id)`. One
  profile serves models that differ: Kimi may need reasoning echoed back,
  and gpt-oss rejects a forced tool call.
- **`forced_tool_choice` capability:** declared false, a forced tool call
  (`required`, or one named tool) is sent as `auto` and the response is
  marked `core_models.tool_choice_downgraded`.
  - The built-in `vertex-maas` profile lists gpt-oss-20b and gpt-oss-120b
    with it, per Google's function-calling notes.
  - mast's final-report path forces a named `finish_task` call, which is
    the case this keeps working.
- **`openaichat.Client.ModelWith`:** builds a model with per-model options.
- **Conformance corpus:** grows to six servers, adding Kimi K2 Thinking,
  Qwen3 Coder 480B, GLM 5.2 and Llama 4 Maverick (`us-east5`) on Vertex AI.
  The existing recordings were re-recorded.

### Changed

- **`tool_choice: "auto"` default:** sent whenever tools are offered and the
  caller expressed no choice, instead of leaving it to the server. Qwen on
  Vertex AI documents worse tool calling when it is unset.

### Fixed

- **Vertex AI errors:** wrapped in a one-element JSON array, they are now
  parsed into `APIError` with a clean message instead of raw JSON.

## v0.1.0 (2026-10-09)

First release: the L0 foundation and the first dialect (L1, core-models half).
Modules: `github.com/go-steer/core-models` v0.1.0,
`github.com/go-steer/core-models/adkv1` v0.1.0 and
`github.com/go-steer/core-models/adkv2` v0.1.0. Pre-1.0: a minor version may
break the API.

### Security

- **`golang.org/x/net` v0.60.0** for GO-2026-6611, -6612 and -6617, and
  **`toolchain go1.26.9`** for the matching standard-library fixes.
  Importers need Go 1.26.9 or later in their own builds to be clear of the
  standard-library half.

### Added

- **`coremodels.Open`:** turns a profile into a `Provider` whose `Model(ctx,
  id)` is an `llm.LLM`. Unbuilt dialects and unserved models are refused
  before any request.
- **`dialect/openaichat`:** the OpenAI Chat Completions adapter, on net/http
  with its own wire types.
  - Handles tools in both genai schema spellings, tool-call id matching
    across providers, and streaming with tool-call fragments.
  - Reads reasoning from `reasoning_content` or `reasoning`, and splits
    inline `<think>` blocks.
  - Maps usage without inventing zeros, uses the requested id as the
    pricing key and keeps the served one, and returns typed `APIError`s.
- **`toolwire`:** the shared check that every declared tool reaches the wire
  with its whole schema. Ported from mast's `internal/toolcatalog`.
- **`profile`:** `reasoning_format: think_tags`, set on the built-in
  `ollama` profile.
- **Conformance corpus:** real exchanges from Ollama 0.9.6 (`qwen3:1.7b`)
  and Vertex AI partner models (`openai/gpt-oss-20b-maas`), replayed
  offline in presubmit. The live smoke (`-tags live`) records new ones.
- **`auth`:** resolves a profile's credential when the profile is opened:
  an API key or bearer token from a named variable, Google ADC, or none.
  `BearerTransport` fetches a fresh token per request. Secrets never appear
  in errors or `String()`.
- **`profile`:** the profile schema with `Validate`, which reports every
  problem at once, and `Resolve`, which fills `${VAR}` and `{param}`
  placeholders and finds credentials before any request.
  - `extends` starts a profile from a built-in.
  - `Find` looks up declared profiles before the built-ins.
  - `DecodeJSON` rejects unknown keys.
  - Built-ins: `vertex-maas`, `ollama`, and the `vllm`, `sglang` and
    `openai-compatible` templates.
- **`callctx`:** the per-call context markers adapters and products share:
  side call, no built-ins, no prompt cache, and the prior-success record.
  They come from core-agent's `pkg/models`, so its helpers can become
  aliases.
- **`retry`:** `Policy.Transport`, an `http.RoundTripper` that retries 408,
  429 and 5xx gateway errors, and any transport failure except a canceled
  context or a certificate error.
  - It honors `retry-after-ms`, `retry-after` (seconds or HTTP date) and
    `x-should-retry`.
  - It hands back, without waiting, a response whose server asks for longer
    than `MaxHeaderDelay`.
  - Each call's retries are recorded on a `Record` for the adapter to stamp
    onto the response.
- **`llm`:** the provider contract (`LLM`, `Request`, `Response`). It mirrors
  ADK's `model` package field for field and imports no ADK, so one core serves
  both ADK majors.
- **`usage`:** `Detail`, the normalized usage record adapters attach beside
  genai's usage metadata. Counts are pointers, so "not reported" stays distinct
  from zero. `FromMetadata` reads it live and after a JSON round trip.
- **`adkv1` and `adkv2` modules:** `Wrap` and `FromADK` adapt the contract to
  ADK v1 (core-agent) and ADK v2 (mast). Reflection tests fail the build if ADK
  adds a field the mirror lacks.
- **CI and tooling:**
  - presubmits for build, vet, gofmt, golangci-lint, `go mod tidy`,
    govulncheck, shell-lint and agent attribution;
  - two checks specific to this repo: the core module imports no ADK, and the
    two adapter modules stay in lockstep.
- **Docs site** under `docs/site`, deployed to
  <https://go-steer.github.io/core-models/>.
