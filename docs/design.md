# core-models: one provider layer for mast and core-agent

**Status:** design, 2026-10-08. The decisions in [§2](#2-settled-decisions) were
settled with the maintainer on that date. Everything else is the proposed shape,
and it changes as the open questions in [§13](#13-open-questions) are answered.

**Inputs:**
- [mast `docs/model-support-design.md`](https://github.com/go-steer/mast/blob/main/docs/model-support-design.md)
  (2026-08-18). That document's **requirements**, R1–R8 and the P0/P1/P2 target
  lists, carry over unchanged. Its §4 design is restated here at library scope,
  and its §9 ("which side of the fork") is answered by this document.
  That document's §10 (the `charmbracelet/fantasy` survey) is the reason this
  library exists rather than an import; see [§3.1](#31-prior-art-why-not-fantasy).
- [mast #312](https://github.com/go-steer/mast/issues/312) — "earn the plural":
  the third real provider backend, OpenAI-compatible first, post-v1.0. This
  library is how #312 gets done without doing it twice.
- Two inventories of the current provider layers, taken 2026-10-08 against mast
  `57ef9d8` and core-agent `9ad397d0`
  ([§12](#12-findings-from-the-inventory)).

---

## 1. Why a library

Today mast and core-agent each carry a copy of the Gemini and Anthropic
adapters. mast's copy was ported from core-agent `b8dd225` and now lives under
`internal/providers/`. Since then the two copies have drifted in both
directions, and `docs/sibling-sync.md` spends a large share of its length
reconciling them:

- **core-agent is ahead on Anthropic prompt caching.** It has rolling and
  1-hour-TTL caching, with the 1-hour share carried in its cache-write sidecar,
  and Gemini-specific retry handling: a token bucket, and a retry on a bare 400
  after an earlier call succeeded.
- **mast is ahead on the record shape.**
  - Its pointer-valued `usage.Detail` sidecar (#352) tells "not reported" apart
    from zero.
  - Its `modelretry` decorator (#452) applies one retry posture at `model.LLM`
    across both vendors.
  - Its adapters use option structs, have no registry, and import nothing else
    from mast except the usage sidecar's `pkg/budget` seam.
- **Each repo fixed a different half of the same problems.** Vertex cache
  eviction is one example: mast #325 and core-agent #902 fixed it in different
  places.

The requirement is OpenAI first-party, Vertex MaaS partner models, and
self-hosted vLLM, SGLang and Ollama. Building that twice, or building it once and
porting it through `sibling-sync.md`, would make the drift worse.

`core-models` owns the provider-intrinsic layer **once**. mast and core-agent
import it. Product policy stays in each product.

## 2. Settled decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Repo `github.com/go-steer/core-models`** | Matches the `core-agent` / `core-tui` family and is neutral to both consumers. |
| D2 | **The core module depends on genai and vendor SDKs only, never ADK. Two nested shim modules, `adkv1` and `adkv2`, adapt it to each ADK major.** | mast is on `adk/v2` and core-agent is on `adk` v1.7. Their `model.LLM`, `LLMRequest` and `LLMResponse` are identical in fields but are distinct Go types. The shims are field copies. The `adkv1` shim is deleted whenever core-agent moves to ADK v2; that migration is a separate track and does not block this work. |
| D3 | **Pricing moves into the library, in a later phase (L6)** | The two catalogs diverged in opposite directions: mast keys rates by (backend, model), and core-agent has the 1-hour cache-write rate. They are merged once, after the adapters land. Until then each repo prices new providers through its own catalog's override layer. |
| D4 | **New providers first, then extract the existing ones** | New providers are the requirement. The contract is still designed against the existing Gemini and Anthropic adapters ([§4](#4-the-contract)), so extracting them later is a move, not a redesign. |
| D5 | **The library does not use ADK's `model/openaimodel`** *(amended in L1: `openai-chat` is written on net/http and its own wire types, not openai-go; see §7.1)* | It now speaks both Responses and Chat Completions, but it exists only in ADK v2, so core-agent cannot use it (D2). The OpenAI dialects are written on `openai-go/v3` directly. That module is already in mast's module graph through ADK v2. |
| D6 | **No proxy as a required hop** | Carried over from mast model-support §4.7. LiteLLM or OpenRouter is reachable as an ordinary `openai-chat` profile; it is never in the path by default. |

## 3. Module layout

```
github.com/go-steer/core-models              # core module: genai + vendor SDKs, no ADK
  llm/            Request, Response, LLM interface (mirror of ADK's model types)
  profile/        Profile schema, built-in profiles, Open(ctx, Profile) → Provider
  auth/           credential sources: api_key(env), google_adc, bearer(env), none
  usage/          Detail (normalized usage record) + metadata (de)serialization
  callctx/        shared context vocabulary (side call, opt-outs, prior success, retry key)
  retry/          HTTP-layer, header-aware retry + transient classification (core-agent retry.go, mast modelretry, fantasy retry.go)
  dialect/
    openaichat/   /v1/chat/completions   (L1)
    openairesp/   /v1/responses          (L2)
    anthropic/    Messages: first-party, Vertex, later Bedrock   (L4)
    gemini/       genai generateContent: API key, Vertex; + vertexcache  (L5)
  kvmetrics/      opt-in Prometheus scrape for self-hosted servers (L3)
  pricing/        Rates, catalog, LiteLLM regen keyed by (backend, model)  (L6)
  toolwire/       Wire + Verify invariant (from mast internal/toolcatalog/verify.go)
  conformance/    per-dialect recorded-turn corpus + replay harness
  adkv1/  (go.mod) google.golang.org/adk  v1.x  — Wrap(llm.LLM) model.LLM, grounding projection
  adkv2/  (go.mod) google.golang.org/adk/v2     — same
```

**Nested modules, not subpackages.** If the core `go.mod` required both ADK
majors, both would land in every consumer's module graph. With nested modules,
mast requires `core-models` + `core-models/adkv2`, and core-agent requires
`core-models` + `core-models/adkv1`. Tags are per module (`v0.1.0`,
`adkv2/v0.1.0`). Local development across the three repos uses `go.work`.

### 3.1 Prior art: why not fantasy

[`charmbracelet/fantasy`](https://github.com/charmbracelet/fantasy) is already a
standalone Go multi-provider library, with both OpenAI dialects, openaicompat,
Azure, Bedrock, Google and Anthropic. mast model-support §10 measured it
(v0.43.0, 2026-09-13) and declined it as a runtime dependency. The reasons
carry over to `core-models` unchanged:

- **Its contract is its own `LanguageModel` and agent loop.** Both products'
  contract is ADK's `model.LLM` over genai, so fantasy adds a lossy middle hop.
- **`fantasy.Usage` holds plain `int64`s**, so "not reported" and zero look the
  same. That loses the distinction R3 depends on.
- **It forces dependency changes:**
  - It moves the genai and anthropic-sdk-go versions underneath the existing
    adapters.
  - It raises the Go floor to 1.27.
  - It adds a second JSON-schema library and a second YAML library.

`core-models` is the library fantasy would have been with an ADK/genai-shaped
contract. It lifts what §10.2 named, with Charm's copyright and `NOTICE`
carried on any derived file (§10.4):

- the `providertests/` shape: one suite run against every provider, recorded
  with VCR;
- `retry.go`'s header-aware backoff;
- `openaicompat`'s list of ways an "OpenAI-compatible" server differs from
  OpenAI.

That list is direct input to [§5](#5-profiles)'s capability vocabulary.

### 3.2 What stays in each product

**What stays in each product.** CLI flags and config-file discovery, the
auto-detect precedence, which models each tier promotes
(`taskclass.ModelForTier`), compaction thresholds, budget meters and usage
trackers, event-log rebuild, telemetry labels, test fakes (echo, toolactor,
scripted), and `RatePer1K`'s invented fallbacks. The last item should be deleted
rather than moved ([§9](#9-pricing-l6)).

## 4. The contract

```go
package llm

// LLM mirrors ADK's model.LLM field-for-field so the adk shims are copies.
type LLM interface {
    Name() string
    GenerateContent(ctx context.Context, req *Request, stream bool) iter.Seq2[*Response, error]
}

// Request mirrors model.LLMRequest minus Tools (map[string]any of ADK tool
// objects, which adapters never read: the wire tools come from Config.Tools).
type Request struct {
    Model    string
    Contents []*genai.Content
    Config   *genai.GenerateContentConfig
}

// Response mirrors model.LLMResponse.
type Response struct { /* Content, UsageMetadata, CustomMetadata, ModelVersion,
    Partial, TurnComplete, FinishReason, ErrorCode, ErrorMessage, ... */ }
```

```go
package profile

type Provider interface {
    Name() string                 // profile name: "openai", "vertex-maas", "house-vllm"
    Backend() string              // pricing/identity key (D3, mast W10.0's backend names)
    Model(ctx context.Context, id string) (llm.LLM, error)
    DefaultSmallModel() string    // "" when the profile has no small tier
    Capabilities() Capabilities
    BuiltinToolNames() []string   // core-agent's BuiltinToolsReporter, generalized
}

func Open(ctx context.Context, p Profile, opts ...Option) (Provider, error)
```

Three properties are part of the contract rather than conventions:

1. **Unresolvable fails at construction** (R1). `Open` validates the profile:
   dialect known, auth resolvable, base URL well formed, model id listed or the
   profile is open-ended. The error names the profile.
   `Model(ctx, id)` refuses an id the profile cannot serve.
2. **`ModelVersion` is a key that resolves.** Every adapter stamps the served
   model if the server echoes one that the identity rule accepts; otherwise it
   stamps the requested id. This is the #210 / #829 rule generalized. The
   identity rule is per profile: a Vertex resource path, an Azure deployment
   name or a vLLM weights path is never stamped raw. The served string goes to
   `usage.Detail.ServedModel` instead, which closes mast model-support §3's
   tenth seam.
3. **genai `UsageMetadata` stays populated.** Every existing consumer path keeps
   working unchanged. The richer record rides beside it ([§6](#6-usage-one-normalized-record)).

The `adkvN` shims provide `Wrap(llm.LLM) model.LLM`, `WrapProvider`, and the ADK
pieces that cannot live in the core. The main such piece today is the Gemini
`GroundingProjection`, which builds `session.Event`s, and `session.NewEvent`'s
signature differs between v1 and v2.

## 5. Profiles

A profile is a declared record of endpoint, credential, dialect, capabilities
and models. **The library owns the schema and the built-in profiles. Each
product decides where operator profiles are read from.** mast's OQ-1 was
`.agents/providers.yaml` versus `config.json`; that choice becomes a per-product
config-layout decision over a shared schema.

```yaml
name: house-vllm
dialect: openai-chat            # openai-chat | openai-responses | anthropic | gemini
base_url: http://vllm.infra.svc:8000/v1
auth: {kind: none}              # api_key{env} | google_adc | bearer{env} | none
backend: house-vllm             # defaults to name
open_models: true               # self-hosted: the operator names the model
capabilities:
  response_schema: true         # honors response_format json_schema
  reasoning_echo: false
  parallel_tool_calls: true
  server_tools: false
usage: {cached_tokens: unreliable}   # suppress a known-bad field (vLLM V1 bug)
metrics_url: http://vllm.infra.svc:8000/metrics   # opt-in, §8
models:
  - {id: Qwen/Qwen3-Coder-Next, tier: mid, context_window: 262144}
tiers: {mid: Qwen/Qwen3-Coder-Next}
```

**As built in L0** (`profile/`), with four refinements to the sketch above:

- `extends: <built-in>` starts a profile from a template.
- `base_url` takes `{param}` placeholders filled from `params`, whose
  values may be `${VAR}` or `${VAR:-default}`.
- Capabilities are tri-state (`*bool`), so an operator can override a
  template's `true` with `false`.
- `rates:` waits for the price catalog (L6).

`Open` (profile to `Provider`) lands with the first dialect in L1. Until
then, `Resolve` is the construction-time check.

**Built-in profiles.** Each is shipped as Go data and pinned by tests.

| Profile | Dialect | Auth | Phase |
|---|---|---|---|
| `openai-compatible` (template) | openai-chat | api_key / bearer / none | L1 |
| `vertex-maas` | openai-chat; `base_url` templated on `{project}`/`{region}` | google_adc | L1 |
| `vllm`, `sglang`, `ollama` (templates; Ollama defaults to `http://localhost:11434/v1`) | openai-chat | none | L1 |
| `openai` | openai-responses | api_key `OPENAI_API_KEY` | L2 |
| `xai` | openai-chat (Responses opt-in) | api_key `XAI_API_KEY` | L2 |
| `anthropic`, `anthropic-vertex` | anthropic | api_key / google_adc | L4 |
| `gemini`, `vertex` | gemini | api_key / google_adc | L5 |

**Capabilities refuse at startup** (mast model-support §4.6). The library
exposes `Capabilities()`. A product that needs one, such as mast's `bounded`
shape needing `response_schema`, checks it at composition and refuses, naming
the profile and the capability.

**Environment detection is a helper, not a default.** `profile.Detect(env)`
returns the candidate built-in profiles the environment can satisfy, in a
documented order. core-agent's auto-detect and mast's `anthropicBackend` probe
become thin calls to it, with each product keeping its own precedence.

## 6. Usage: one normalized record

**The base is mast's shipped `internal/providers/usage.Detail`** (#352, M0,
2026-09-13). It uses pointer-valued counts with `omitempty`: a field the
provider never reported is omitted from the persisted event, and a reported
zero is written as zero. The library extends it with the fields #352 left out
on the grounds that no adapter could fill them yet. Each new field arrives with
the phase that first populates it.

```go
package usage

const MetadataKey = "core_models.usage"   // in llm.Response.CustomMetadata

type Detail struct {
    // As shipped in mast #352:
    CacheReadTokens   *int64 `json:"cache_read_tokens,omitempty"`
    CacheWriteTokens  *int64 `json:"cache_write_tokens,omitempty"`    // all TTLs
    ReasoningTokens   *int64 `json:"reasoning_tokens,omitempty"`
    ToolUseTokens     *int64 `json:"tool_use_tokens,omitempty"`
    ServedModel       string `json:"served_model,omitempty"`
    ProviderRequestID string `json:"provider_request_id,omitempty"`

    // Added by core-models, each with the phase that first fills it:
    CacheWrite1hTokens *int64 `json:"cache_write_1h_tokens,omitempty"` // L4, core-agent #770
    Backend            string `json:"backend,omitempty"`               // L1: the pricing key
    Region             string `json:"region,omitempty"`                // L1 (Vertex MaaS)
    CachedTokensSuppressed bool `json:"cached_tokens_suppressed,omitempty"` // L3, profile flag
}

func Attach(resp *llm.Response, d *Detail)
// FromMetadata accepts the typed value (live) or a map[string]any (decoded
// from an event log).
func FromMetadata(md map[string]any) (*Detail, bool)
```

**Consumers translate at their own seam, because the library cannot import
them.**

- **mast:** `pkg/budget` is frozen at v1.0 and must import nothing (#338). The
  meter reads `CustomMetadata["mast.usage_detail"].(budget.Detailer)`.
  - mast's `adkv2` glue therefore re-keys the library's record into mast's own
    `usage.Detail`, which keeps implementing `budget.Detailer`. That costs one
    decorator in `internal/compose` and changes nothing in budget.
  - `internal/providers/usage` stays as a thin alias plus the `Detailer`
    method.
- **core-agent:** `TurnUsageFromMetadata` reads `Detail` first, then falls back
  to the legacy sidecar keys (`cache_creation_input_tokens`,
  `cache_creation_1h_input_tokens`). Library adapters keep writing those keys
  until core-agent's reader has switched, so `usage.Rebuild` over old event
  logs keeps working.
- **Clamping stays in each product's meter.** That covers cached tokens
  exceeding prompt tokens and mast's `fitBucket`: a guard written once per
  meter, not per adapter.

## 7. Dialects

### 7.1 `openai-chat` (L1): the leverage

**As built** (`dialect/openaichat`, L1): on net/http and the package's own
wire types rather than openai-go. These servers are OpenAI-shaped, not
OpenAI, so the adapter does three things an SDK's types get in the way of:

- **Pointer fields** keep a usage count the server never sent distinct
  from a zero it did, which `usage.Detail` requires.
- **Non-standard fields** (`reasoning_content`, `reasoning`) are read and
  echoed natively.
- **No new module**, and so no Azure or AWS entries in `go.sum`.

fantasy needed around 600 lines of hooks to bend openai-go around the same
servers. openai-go remains the likely base for `openai-responses` (L2),
where the server is OpenAI.

The first live runs found one quirk the table below did not have. Ollama
0.9.6 puts a reasoning model's thinking inline as a leading
`<think>…</think>` block in `content`, so profiles gained
`reasoning_format: think_tags`. The built-in `ollama` profile sets it. The
adapter splits the block into a thought part, chunk-boundary safe.

One adapter reaches Vertex MaaS, xAI, vLLM, SGLang, Ollama, llama.cpp, NIM, the
managed long tail, and any LiteLLM or OpenRouter endpoint.

| Concern | Mapping |
|---|---|
| Tools | genai `FunctionDeclaration` becomes `tools[].function`. The schema comes from `ParametersJsonSchema` if set, otherwise from `Parameters` converted to JSON Schema. Both spellings are covered by `toolwire.Verify` from day one: the #154 lesson. |
| Tool calls | `tool_calls[]` (id, name, arguments JSON) ↔ `genai.FunctionCall{ID, Name, Args}`. `FunctionResponse` becomes a `role: tool` message with `tool_call_id`. Ids are synthesized if a server omits them, and stay stable within a turn. |
| Streaming | Requests `stream_options.include_usage`. Accumulates `tool_calls[i].function.arguments` deltas by index and emits partial text events only. The final event carries the full content, usage and finish reason. |
| Finish reasons | `stop`, `length` (becomes `MAX_TOKENS`), `tool_calls`, `content_filter` (becomes `SAFETY`). Unknown values map to `OTHER` and keep the raw string in `CustomMetadata`. |
| Structured output | Sends `response_format: {type: json_schema}` only if `capabilities.response_schema` is true; otherwise refuses ([§5](#5-profiles)). |
| System prompt | `SystemInstruction` becomes a leading `system` message. |
| Reasoning | Reads `reasoning_content` (vLLM, DeepSeek, SGLang), surfaced as `Part{Thought: true}`. It is **not** echoed back unless `reasoning_echo` is set; the Chat dialect drops reasoning silently, and the flag declares that. |
| Usage | `prompt_tokens`, `completion_tokens`, `prompt_tokens_details.cached_tokens`, `completion_tokens_details.reasoning_tokens`. Absent fields stay nil, and the profile's `usage:` overrides can force a field to "not reported". |
| Retry | `openai-go`'s own retries are turned **off** (`MaxRetries: 0`). The adapter instead runs the shared `retry` package at the HTTP layer, where `retry-after-ms` and `retry-after` are still visible; a `model.LLM` decorator can't see them, as mast's `modelretry` doc notes. Retries are recorded under `callctx.RetryMetadataKey`, which is core-agent #1206 generalized. Products keep their own outer posture (mast's `modelretry`, core-agent's `RetryPolicy`). [Q7](#13-open-questions) is whether those collapse into the library's. |
| Vertex MaaS | Base URL templated from project and region, an ADC token source as an `oauth2` transport, publisher-qualified ids (`deepseek-ai/…-maas`), and `Backend()` set to `vertex-maas` so prices can never fall through to first-party rows. |

### 7.2 `openai-responses` (L2)

Used for OpenAI first-party, and for xAI as an opt-in. Requests are sent with
`store: false` and `include: ["reasoning.encrypted_content"]`. Encrypted
reasoning round-trips through `genai.Part.ThoughtSignature`, mirroring how the
Anthropic adapter carries thinking signatures, so a tool loop does not lose its
reasoning. Usage maps `input_tokens_details.cached_tokens` and
`output_tokens_details.reasoning_tokens`.

### 7.3 `anthropic` (L4) and `gemini` (L5): extraction

The base is mast's API shape: option structs, no registry, and the
`usage.Detail` sidecar.

**Kept from mast:**
- the thinking-request shape chosen per model id (#369/#373: `adaptive` from
  the 4-6 generation on, and an unknown model takes the newer shape);
- the shared eviction verdict, `vertexcacheerr` (#325), with both the wrapper
  and the manager asking it;
- the per-backend built-in gating (#340).

**Ported onto it from core-agent:**
- **Anthropic:** rolling and TTL prompt caching (`cache.go`, `CacheOptions`),
  and the 1-hour cache-write share, written as `Detail.CacheWrite1hTokens`.
- **Gemini:**
  - setting `IncludeServerSideToolInvocations` from the backend automatically
    (this closes the mast finding in [§12](#12-findings-from-the-inventory));
  - the transient classification and bare-400 retry (#1247), which feed the
    shared `retry` package rather than a third policy.
- **vertexcache:** `IsBelowCacheMinimum` (#1067).
- **Both adapters:**
  - the positional schema normalizer (#532), which avoids mast's generic walk
    rewriting instance data;
  - the per-request opt-outs through `callctx`.

The `*config.Config` constructors become option structs. core-agent keeps a
small `pkg/models` facade that maps its `config.ModelConfig` onto profiles, so
its config surface does not change.

Claude on Bedrock rides on L4 through `anthropic-sdk-go/bedrock`, and Azure
OpenAI rides on L1 and L2 through `openai-go/azure`. Both are P1, after L5.

## 8. Self-hosted: tokens always, KV statistics opt-in (L3)

These are the minimum and the extra from the ask, unchanged from mast
model-support §4.4:

1. **Tokens come from the response** on every server, through `openai-chat`
   usage. When cached tokens are missing they render as *not reported*. A
   profile sets `usage.cached_tokens: unreliable` for vLLM V1 builds with the
   `prompt_tokens_details: null` bug.
2. **KV statistics come from `kvmetrics`.** It reads `metrics_url` in Prometheus
   text format. A `kvmetrics.Sampler` takes a snapshot at two points chosen by
   the caller and returns a `Window`: prefix-cache hits/queries and hit rate,
   plus peak KV utilization.
   - Built-in parsers: vLLM (`vllm:prefix_cache_{queries,hits}`,
     `vllm:kv_cache_usage_perc`), SGLang (`sglang_cache_hit_rate`, `hicache_*`)
     and llama.cpp.
   - The result is labeled **fleet-level and time-correlated**. It never enters
     a cost figure and is never attributed to a specialist.
   - Ollama has no per-request KV statistics. The docs say so; the library
     doesn't invent any.

The library provides the sampler and **each product decides when to sample**:
at session start and end for mast, and per `/stats` for core-agent. That answers
mast OQ-4 by splitting the work: a library embed has no Prometheus, and an
operator with Prometheus can ignore the sampler.

## 9. Pricing (L6)

The mechanics from mast model-support §4.5, merged once:

- mast's `LookupFor(backend, model)` and `builtinByBackend`, plus core-agent's
  `CacheCreation1hInputPerMTok` / `CostUSDWithCacheTTLs`.
- The LiteLLM regen, with the `/`-drop rule rehabilitated into a per-backend
  prefix table (`vertex_ai/`, `xai/`, `azure/`, …).
- `LongContext{ThresholdTokens, InputMult, OutputMult}` for OpenAI's 272K cliff.
- **Self-hosted is unpriced by default:** `$—`, never `0.001`. Declared `rates:`
  in a profile feed the operator-override layer and are labeled
  `operator declared`.

Products keep their file paths (`~/.mast` vs `~/.core-agent`), refresh policy,
and config-override wiring. `compose.RatePer1K`'s invented `default: 0.001` is
deleted in mast rather than moved.

**Before L6:** new providers are priced through each product's existing catalog
override, or are explicitly unpriced. A `max_cost_usd` ceiling on an unpriced
model fails at startup (R6) in both products.

## 10. Testing

- **The conformance corpus is the gate** (R2/R8). Each dialect has recorded HTTP
  exchanges replayed through `httptest`, credential-free in presubmit. The
  corpus covers a tool call, parallel tool calls, a multi-turn tool loop, a
  reasoning round-trip, a refusal, a max-tokens stop, a cache hit, a
  missing-usage-field case, and a streaming interruption. The precedents are
  mast `pkg/attach/testdata/conformance` and the inline SSE fixtures in both
  repos' `llm_offline_test.go`.
- **`toolwire.Verify` moves into the library** (from mast
  `internal/toolcatalog/verify.go`, standard library only), along with the MCP
  stub. Each dialect has a toolwire test against a library-side catalog.
  **mast and core-agent each keep a downstream toolwire test** that drives their
  real catalog through the library adapters, because the catalog is captured
  from the product's own rigs and cannot move.
- **Shim tests:** each `adkvN` runs one ADK runner end to end over a recorded
  `openai-chat` turn, so a field missing from the copy cannot go unnoticed.
- **Live smoke:** behind build tags, per profile, run nightly where credentials
  exist. A provider is called **supported** only after a J-tier live run with
  tool-calling metrics at parity with the Claude baseline (mast #168–#172).
  Profiles without that run ship as *documented templates, explicitly
  unvalidated* (mast OQ-5).

## 11. Phases

| Phase | Work | Exit criterion |
|---|---|---|
| **L0** | Repo, CI, the `llm`/`usage`/`callctx`/`retry`/`auth`/`profile` skeleton, `adkv1` + `adkv2` shims, `toolwire`, a conformance harness | Both shims pass a round-trip test with a fake `llm.LLM`; mast and core-agent build against the library through `go.work` with no behavior change |
| **L1** | `openai-chat` and the profiles `openai-compatible`, `vertex-maas`, `vllm`, `sglang`, `ollama` | Conformance corpus green offline. **mast adopts it:** mast's M1 profile wiring resolves `--provider <profile>`, and a vLLM profile runs the triage bundle end to end with tokens per turn and `$—`. A live run against Vertex MaaS (gpt-oss-20b) and a local Ollama |
| **L2** | `openai-responses`, plus the `openai` and `xai` profiles | Encrypted-reasoning round-trip in the corpus; live tool loop against OpenAI and xAI |
| **L3** | `kvmetrics` sampler with vLLM, SGLang and llama.cpp parsers; the `cached_tokens: unreliable` flag | mast `/usage` shows a session-scoped prefix-cache hit rate labeled fleet-level, against a real vLLM |
| **L3'** | **core-agent adopts L1–L3** through `adkv1`: `models.Register` gains profile-backed providers, `config.json` gains a profile section, and `--provider` opens up | core-agent `--provider vllm --model …` drives a tool loop; existing Gemini and Anthropic paths unchanged |
| **L4** | Extract Anthropic (first-party and Vertex), merging in core-agent's caching and sidecar; both products switch | Both products' toolwire and cache-accounting tests green on the library adapter, including mast's #352 measured-turn fixture and core-agent's 1-hour-TTL accounting; core-agent prompt caching available to mast |
| **L5** | Extract Gemini and vertexcache, merging in core-agent's retry and cache handling; both products switch | mast's `IncludeServerSideToolInvocations` finding closed ([§12](#12-findings-from-the-inventory)); mast's #325 and core-agent's #902 eviction tests both green on the one verdict; core-agent's retry tests green on the library |
| **L6** | Pricing merge ([§9](#9-pricing-l6)) | One catalog; mast's backend-shape tests and core-agent's 1-hour-TTL tests both pass against it |
| **P1** | Bedrock Claude, Azure OpenAI, long-tail built-in profiles, NIM | Each has a tier map, catalog rows and a live smoke, or ships as an unvalidated template |

**Sync discipline from L0 on:** provider changes land in `core-models` and are
consumed by version bump. Neither product edits a vendored copy. mast's
`sibling-sync.md` gets one row per extracted package marking it
"owned by core-models"; the existing ledger rows for those packages are closed
out at that point.

## 12. Findings from the inventory

- **mast: the Gemini API-key backend never sets
  `IncludeServerSideToolInvocations`.** `internal/compose` calls
  `geminiprov.Wrap` without the option.
  - The adapter's own doc comment says Gemini 3+ rejects built-ins combined with
    function tools unless the flag is set. core-agent sets it from the backend.
  - Since #340 built-ins are off unless a bundle asks for them, so the failure
    needs a bundle that turns on `builtin_tools` and runs on the Developer API
    rather than Vertex.
  - Filed as [mast #505](https://github.com/go-steer/mast/issues/505); L5 closes it structurally.
- **mast model-support §4.1 and M3 are stale on ADK.** They describe
  `openaimodel` as Responses-only and propose wrapping it. ADK v2.5 has Chat
  Completions too, and D5 rules it out for the library either way.
- **Not findings, checked and set aside:**
  - mast's Vertex context-cache manager has no caller. That is deliberate and
    documented (`DESIGN.md`, and the docs site's "built, and nothing wires it").
  - The sibling-sync ledger rows for the provider packages were re-triaged
    upstream (#333, #453) after the snapshot that first suggested they were
    stale.

## 13. Open questions

- **Q1. Where each product reads operator profiles** (mast OQ-1). The schema is
  shared; mast's config-layout-design and core-agent's `config.json` each pick a
  location.
- **Q2. Do profile-declared tiers outrank the products' Go tier tables, or only
  extend them** (mast OQ-2)? This is product policy, but both products should
  answer the same way.
- **Q3. How much long-context, regional and batch price modeling** (mast OQ-3).
  It needs an answer before L6.
- **Q4. First real self-hosted target** (mast OQ-6). This design assumes vLLM;
  if the first deployment is SGLang, the corpus's validation weights follow it.
- **Q5. When core-agent moves to ADK v2.** It is not required, but it deletes
  `adkv1` and lets the library reconsider wrapping ADK's Gemini model.
- **Q6. Release cadence and governance:** the repo is public like its siblings.
  CODEOWNERS and the release cadence relative to the two products are still
  open.
- **Q7. One retry posture or three.** mast's `modelretry` (one retry after 2s,
  process-wide cooldown) and core-agent's `RetryPolicy` (token bucket, burst 3)
  are both product-level today. Once the dialects retry at the HTTP layer,
  should those outer policies shrink to "what to do after the library gave up",
  or stay as they are?
- **Q8. mast's v1.0 freeze (#300) and a v0.x dependency.** `internal/providers`
  is outside the frozen surface, so importing a pre-1.0 `core-models` does not
  break the promise. Should mast still require core-models v1 before mast v1.x
  names it in any exported type? This design assumes no exported mast type ever
  mentions a core-models type.
