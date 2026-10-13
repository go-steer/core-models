# Changelog

## Unreleased

## v0.13.0 (2026-10-13)

### API Change

- **`openai-chat` refuses generation settings it can't send instead of
  dropping them** (#47). Before, a caller asking for `top_k`, logprobs,
  safety settings, a Gemini cached-content reference, non-text modalities,
  labels, routing, a service tier or a non-JSON MIME type got a request
  sent without it, and an answer to a different question. Now the call
  fails before anything is sent, naming the setting. It's the same list
  `openai-responses` has refused since it shipped, minus stop sequences,
  seed and penalties, which Chat Completions carries. `top_k` for a server
  that accepts it, such as vLLM, goes in the profile's `extra_body`.
  Thinking settings are unaffected. Neither mast nor core-agent sets any
  refused field on a model request (checked before this change).

## v0.12.0 (2026-10-13)

### Added

- **`pricing.IntroductoryRates()` and `IntroductoryRate.Check`.** The list
  of known promotional rates with end dates, and the check that fails once
  a row outlives its expiry, or inverts to pin a `Resolved` rate. It was a
  test-only table that mast and core-models each kept. Exported, a product
  checks the rows it bills from against one list, and its build goes red on
  a lapse whatever core-models version it pins. core-models' own test now
  covers backend-qualified rows (e.g. `vertex/gemini-3.8-flash`) as well as
  bare ones.

### Documentation

- **Claude on Anthropic's first-party API is recorded live**
  (`testdata/conformance/anthropic-claude-haiku-4-5`, with a workspace-scoped
  key), next to the Vertex AI recording. That closes the last live-test gap:
  every dialect and built-in vendor profile now replays a real recording in
  presubmit.

## v0.11.1 (2026-10-13)

### Fixed

- **xAI usage counts a reasoning reply's answer.** xAI reports reasoning
  tokens beside completion/output tokens (prompt + output + reasoning =
  total); OpenAI reports them inside. The shared OpenAI usage mapping
  assumed OpenAI's convention and subtracted reasoning from output, so an
  xAI reasoning reply's answer tokens came out as zero, or short whenever
  the answer outran the reasoning, and its cost came out low. It now tells
  the two apart by `total_tokens`. Found by the first live run against
  xAI.

### Documentation

- **L2's exit criterion is met.** Live tool loops are recorded in the
  conformance corpus: OpenAI `gpt-5-mini` over Responses, with the
  encrypted-reasoning round trip accepted by OpenAI, and xAI
  `grok-4.20-0309-reasoning` over Chat Completions and over Responses. The
  replay test supplies placeholder first-party keys, since the recorder
  drops headers.

### Documentation

- **mast prices from `pricing`** (mast #544). Its own catalog, regen tool
  and weekly workflow are gone. mast keeps an introductory-rate expiry
  check over `pricing.Builtin()`, sends each core-models pin bump to a
  human with a "what moved" rate diff, and leaves Dependabot's automerge
  off for core-models. core-agent's adoption is still owed.

### Documentation

- **L3 is done.** mast #543 consumes `kvmetrics`: `/usage` reports a
  fleet-level prefix-cache window for the last turn and the session, and
  the judged eval board reports one per incident. Live against our vLLM
  0.31, the hit rates were 85–95%.

## v0.11.0 (2026-10-13)

### Added

- **`pricing`, the price catalog both products share (L6, library half).**
  Merged from mast's `internal/pricing` and core-agent's `pkg/pricing`:
  - `Rates` with 5-minute and 1-hour cache-write rates and the `Cost*`
    methods, plus **`LongContext`**: the above-threshold rates LiteLLM
    publishes (OpenAI's 272K, Gemini's 200K, some Claude 100K), applied to
    the whole call by `Rates.At` and every cost method. 57 builtin rows
    carry one.
  - The layered `Catalog` (config override, project file, user manual,
    user external, builtin, declared), `LookupFor(backend, model)`,
    `...WithSource`, `Counts`. A lookup tries the exact qualified and bare
    keys layer by layer before any prefix, so a higher layer's bare row
    beats a lower layer's qualified one (mast's version did the reverse).
  - **Declared rates**: `DeclaredFromProfiles` turns profiles'
    `models[].rates` into the lowest layer, keyed `<backend>/<model>` and
    labelled `profile-declared`.
  - `Refresh` with `RefreshOptions.MaxBodyBytes` (default 32 MiB). It
    stamps each refreshed row's `UpdatedAt`, and returns an error rather
    than overwrite a `pricing.json` it cannot parse.
  - The generated `builtin.go` and `dev/regen-builtin-pricing`, now also
    emitting `openai` and `xai` rows; mast's 123 rows come out unchanged.
    `.github/workflows/pricing-regen.yml` opens a weekly PR when LiteLLM
    moves.
  - Nothing invents a default rate: an unpriced model is a miss.

  File locations, refresh cadence and config wiring stay with the
  products. Both products switching to it is owed.

## v0.10.0 (2026-10-13)

### Added

- **`kvmetrics`, the opt-in KV-cache sampler (L3).** It reads a self-hosted
  server's Prometheus endpoint (a profile's `metrics_url`) and reports, for
  a window the caller chooses: prefix-cache tokens queried and hit, the hit
  rate, and peak KV utilization, with background polling for the peak.
  - Results are **fleet-level**: the whole server's activity, never
    attributed to a request or folded into usage or cost.
  - A metric the server doesn't expose stays nil.
  - Readers for vLLM (verified live on 0.31), SGLang and llama.cpp; Ollama
    gets `ErrNoMetrics`.
  - Increases are computed per series and summed, so one engine
    restarting mid-window is counted correctly and flagged.
  - The profile's credential is sent only to a metrics endpoint with the
    same origin as the API, and a Google ADC token only over https.
  - A new profile field, `metrics` (`vllm`, `sglang`, `llamacpp`, `none`),
    names the vocabulary. The `vllm`, `sglang` and `ollama` built-ins set
    it, and extending profiles inherit it.

## v0.9.0 (2026-10-13)

### Added

- **The `openai-responses` dialect (L2), package `dialect/openairesp`:**
  OpenAI's Responses API, written on net/http and its own wire types like
  `openai-chat`.
  - **Stateless:** `store: false`, with a reasoning model's encrypted
    reasoning requested and carried forward through
    `genai.Part.ThoughtSignature`, so a tool loop keeps its reasoning.
  - **Mapping:** the system prompt goes in `instructions`; function calls
    and outputs are matched by `call_id`; tools use the flat shape.
    `ThinkingLevel` maps to `reasoning.effort`; a thinking budget is not
    mapped, since what "no reasoning" means differs by model.
    `IncludeThoughts` asks for summaries. Tool allow-lists go in an
    `allowed_tools` choice. Every generation setting the API can't carry
    (stop, seed, penalties, top_k, logprobs, safety settings and more) is
    refused with its name.
  - **Streaming:** the final response is decoded from `response.completed`,
    as a non-streamed one is.
  - **Usage:** cached and reasoning tokens, plus vLLM's `cache_write_tokens`.
  - **New built-in profiles:** `openai` (Responses, `OPENAI_API_KEY`) and
    `xai` (Chat Completions, `XAI_API_KEY`). Any profile can opt into
    Responses with `dialect: openai-responses`.
  - **Tests:** a conformance corpus recorded from vLLM v0.31's
    `/v1/responses` with Gemma 4.
  - **Not yet live-tested against OpenAI or xAI** (no key). The encrypted
    reasoning round trip is tested offline against OpenAI's documented
    shape.
- **`internal/schemajson`** and **`internal/openaishared`:** the schema
  conversion, usage mapping, error parsing and tool-call ids, now shared
  by both OpenAI dialects. openai-chat's behaviour is unchanged.
- **openai-responses corrects itself on refused optional fields.** When a
  non-reasoning model refuses `include`, or a reasoning model refuses
  `temperature` or `top_p`, the 400 is retried once without that field.
  The refusal is remembered per model, and the response lists what was
  left off under `core_models.dropped_params`. Encrypted reasoning is only
  requested where the profile declares `reasoning_echo`.
- **The conformance corpus's `meta.json` takes a `dialect`,** for a server
  recorded on a dialect its built-in profile doesn't default to.

## v0.8.0 (2026-10-13)

### Added

- **`profile.Profile.ContextWindowFor`** (#22) returns the input window a
  profile declares for a model (`models[].context_window`, in the schema
  since L0, which nothing could read before). It's the window as the
  server serves it, so a host can size compaction for a self-hosted model
  served at less than its own window, such as vLLM's `--max-model-len`,
  instead of assuming 128k. The vLLM fixture's example profile declares
  65536 to match its deployments.

- **`gemini.Client.SetContextCache`** (#31) installs Vertex context-cache
  hooks on a Client that's already built. Models built before it pick them
  up on their next call. A host can now make one Client, build its
  `vertexcache.Manager` from `Caches()`, and hook it in, where before it
  needed a second Client: a second credential resolution and connection
  pool. The new `gemini.ContextCache` type bundles the four hooks; the
  `Options` fields still set the initial hooks. A model reads the hooks
  once per call, so a call never mixes old and new.

### Fixed

- **A Claude-on-Vertex error names the endpoint actually called** (#35).
  The SDK's message showed the request as it built it (`…/v1/messages`),
  before the Vertex middleware rewrote it to
  `…/models/<id>:streamRawPredict`. `APIError.Error()` now substitutes the
  URL the response answered.

## v0.7.0 (2026-10-13)

### Added

- **`adkv2.GroundingProjection`** wraps an ADK `session.Service` so an
  event carrying Gemini Google Search grounding is followed by synthetic
  events: one per query and one per web source, authored
  `gemini.AuthorGoogleSearch`, with no role, so they stay out of the
  model's history. It is built on `gemini.GroundingEvidence` and passes
  the context to `session.NewEvent`, so ADK's replay providers name the
  events. Both products carried their own copy; this replaces them.

## v0.6.6 (2026-10-12)

### Deprecated

- **`adkv1`** is deprecated, and this is its last release. core-agent, its
  only user, moved to ADK v2 (go-steer/core-agent#1291) and to `adkv2`
  (#1296). The module's `go.mod` carries a `Deprecated:` comment, so
  `go get` and pkg.go.dev flag it. Projects on `google.golang.org/adk` v1
  should move to ADK v2 and `adkv2`. The directory goes in a later
  cleanup.

### Documentation

- **Roadmap and design status:** L1, L4 and L5 are done. Both mast (#510,
  #521) and core-agent (#1294, #1296) run on the library. L3 has its
  core-agent adoption done; its KV-cache sampler is still owed.

## v0.6.5 (2026-10-12)

### Fixed

- **`auth.ErrNoADC` survives `profile.Resolve` and `coremodels.Open`.**
  `profile.Resolve` wrapped the auth error with `%v`, so
  `errors.Is(err, auth.ErrNoADC)` was false on the path hosts actually
  call. Found by core-agent. Every error `profile.Resolve` passes through
  is now wrapped with `%w`; the messages are unchanged.

## v0.6.4 (2026-10-12)

### Fixed

All three were found by core-agent's review of its switch to the library
(go-steer/core-agent#1296).

- **A header timeout no longer reads as a cancel.** `ErrNoResponse` used to
  wrap the `context.Canceled` its own timer caused, so a host checking
  `errors.Is(err, context.Canceled)` reported a timeout as an operator
  stop. The cause is now kept as text only.
- **A non-streaming call is no longer capped at five minutes.** It gets no
  headers until the whole answer exists, so `HeaderTimeout` was really
  capping generation time. A large-context Gemini summary hit it. The
  Gemini and OpenAI-compatible adapters now mark non-streaming requests
  (`retry.MarkUnary`), and those wait the new
  `Policy.UnaryHeaderTimeout` (default ten minutes, the Anthropic SDK's
  own non-streaming default). The Anthropic adapter always streams on the
  wire, so it's unaffected. `retry.WithHeaderTimeout(ctx, d)` overrides
  the limit for one call. A `Policy` built by hand rather than from
  `Default()` leaves `UnaryHeaderTimeout` at zero, which means no limit.
- **Vertex requests send the quota project again.** genai's transport
  set `X-Goog-User-Project` from the ADC quota project (or
  `GOOGLE_CLOUD_QUOTA_PROJECT`), and core-models' `BearerTransport` didn't.
  A user credential with a quota project was billed and quota-checked
  against the wrong project. It is read once, as Google's transport
  does, and never overrides a header the caller set.

### Added

- **`auth.ErrNoADC`** (#30): missing Application Default Credentials can
  be detected with `errors.Is`. The message text is unchanged.

## v0.6.3 (2026-10-12)

### Added

- **Vertex express mode** (#29): Gemini on Vertex AI with an API key and
  no project. `dialect/gemini.New` takes an `APIKey` on the Vertex backend
  (alone; a project, location or credential beside it is refused), and an
  `api_key` profile with `platform: vertex` routes there. The new built-in
  `vertex-express` reads `GOOGLE_API_KEY` and prices as `vertex`. mast
  supported this through genai's environment selection and carried a
  bridge after switching to the library; that bridge can go.

## v0.6.2 (2026-10-11)

### Fixed

- **The `vertex` profile falls back to `GOOGLE_CLOUD_REGION`** when
  `GOOGLE_CLOUD_LOCATION` is unset, then to `global`. That is mast's
  location order; without it, mast had to patch the profile's params.

## v0.6.1 (2026-10-10)

### Fixed

- **A streamed Gemini call is marked complete once.** `dialect/gemini`
  set `TurnComplete` on the last streamed chunk and on the aggregate
  after it, so a consumer that commits usage on `TurnComplete` counted the
  call twice. core-agent's `usage.TurnTap` did: a live run showed about
  twice the tokens. Only the aggregate is marked now, as in the other
  dialects, and `llm.Response` documents the rule.

## v0.6.0 (2026-10-10)

### Added

- **Gemini adapter (L5).** Package `dialect/gemini` serves Gemini on the Developer
  API and on Vertex AI as `llm.LLM`, built on genai with no ADK. It is the
  extraction of mast's and core-agent's adapters:
  - built-ins (`web_search`, `url_context`, `code_execution`) are off unless
    asked for, and `include_server_side_tool_invocations` is set from the
    backend, which closes mast #505 in the library;
  - empty answers are retried once and then surface as an error wrapping
    the new `llm.ErrEmptyResponse`;
  - Vertex context caching is wired per model (`ContextCacheModel`) and
    used only by the agent's own turns: a side call, a request that opted
    out of built-ins or prompt caching, and a request carrying a tool
    config all run uncached;
  - a cached turn whose cache is gone is re-sent as a full uncached turn,
    built-ins included, and the manager is told which cache
    (`MarkEvictedName`), so a replacement cache is never discarded;
  - optional usage buckets Gemini omits (cache read, thinking, tool-use
    prompt) are left nil, not 0.
- **`dialect/gemini/vertexcache`:** the Vertex explicit-cache manager, with one
  eviction verdict (`Gone`) for the manager and the adapter (mast #325,
  core-agent #902), and core-agent's stop on content below the cache
  minimum (#1067).
- **Built-in profiles `gemini` and `vertex`.** `gemini` takes its key from
  `GOOGLE_API_KEY` or `GEMINI_API_KEY`; `vertex` uses ADC, with the region
  defaulting to `global`. Their backends are `gemini` and `vertex`, the
  names both products price by. `coremodels.Options` gains `BuiltinTools`
  and `Logf`.
- **`retry.Policy.AfterSuccess`** retries an ambiguous error response once
  for a session that has already been served; successful responses are
  never read. Gemini uses it for Vertex AI's bare
  400 `INVALID_ARGUMENT` (core-agent #898, #1247).
- **`auth.Config.AltEnv`:** further variables to try for a key that goes by
  more than one name.
- **The `anthropic` dialect: Claude on the first-party API and on Vertex
  AI** (`dialect/anthropic`, phase L4), with two built-in profiles,
  `anthropic` and `anthropic-vertex`. It merges mast's and core-agent's
  adapters: mast's shape and per-model thinking request, core-agent's
  rolling and one-hour prompt caching. Opening a profile turns caching on
  by default; `Options.PromptCache` changes the policy and
  `callctx.WithoutPromptCache` turns it off for one call. Cache writes,
  and their one-hour share, land in `usage.Detail`.
- `coremodels.Options.BuiltinTools` maps `web_search` for Claude; a name
  Claude has no tool for is an error. `Options.PromptCache` is new.
- **Nested defaults in profile params:** `${A:-${B:-x}}` reads as the first
  of `A` and `B` that is set, else `x`.
- **A Claude recording in the conformance corpus**
  (`anthropic-vertex-claude-haiku-4-5`).
- **`platform` in profiles.** `vertex` routes an `anthropic` profile the way
  Vertex AI serves Claude. `anthropic-vertex` sets it.

### Changed

- **The retry transport also retries 529**, Anthropic's "overloaded".


### Documentation

- ***Tested models* narrows the Gemma 4 vLLM gap to repeated calls.** Over
  half of vLLM's calls repeated an earlier one; on Vertex AI almost none
  did. Lowering the sampling temperature made it worse, so sampling
  defaults are ruled out.

### Fixed

- **The Gemma 4 GKE fixture now uses the model's own chat template.** The
  example template bundled with vLLM 0.31 predates Google's fixes for
  tool-calling loops. With the model's template, Gemma 4 made 382 model calls
  over the parity run instead of 851. *Tested models* has the new row. Vertex
  AI made 135, so part of the gap remains.

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
