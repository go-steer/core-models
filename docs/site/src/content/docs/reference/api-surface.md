---
title: API surface
description: Every public package core-models exports, who imports it, and its proposed status for v1.0.
---

This page lists every public package, as inventoried on 2026-10-15 against
v0.13.3, with a **proposed** status for v1.0:
- **stable:** covered by the v1 compatibility promise;
- **experimental:** may change in a minor release after v1;
- **internal:** should move out of the public API.

The maintainer decides each status. The work is tracked in the
[v1.0 milestone](https://github.com/go-steer/core-models/milestone/1), and the
criteria are in the design's
[§14](https://github.com/go-steer/core-models/blob/main/docs/design.md#14-road-to-v10).

## Packages

The "Imported by" column counts direct imports in each product's main branch.
A product can also reach a package through `coremodels.Open` without importing
it.

| Package | Exports | Imported by | Proposed | Why |
|---|---|---|---|---|
| `coremodels` (root) | `Open`, `Options`, `Provider` | mast, core-agent | stable | The front door: a profile to a provider |
| `llm` | `LLM`, `Request`, `Response`, `ErrEmptyResponse` | mast, core-agent | stable | The contract. Its fields are genai types, by decision (see below) |
| `usage` | `Detail`, `Attach`, `FromMetadata`, `Int64`, `MetadataKey` | mast, core-agent | stable | The normalized usage record (requirement R3) |
| `callctx` | side-call, prompt-cache, built-ins and prior-success markers | core-agent | stable | Small and context-only. The dialects read it, and core-agent sets it |
| `auth` | `Config`, `Credential`, `Kind`, `Options`, `TokenSource`, `QuotaProjecter`, `ErrNoADC`, `CloudPlatformScope` | mast, core-agent | stable | Credential kinds are part of the profile schema |
| `profile` | `Profile`, `Model`, `Capabilities`, `Resolve`, `Resolved`, `Find`, `Expand`, `Builtin`, `BuiltinNames`, `DecodeJSON`, the enum types, `ReservedBodyKeys`, `Has` | mast, core-agent | stable, with two names to resolve | The schema both products read. `ReservedBodyKeys` is an exported mutable slice, and `Has` is a one-line `*bool` helper. `Tier`'s meaning waits on [Q2](https://github.com/go-steer/core-models/issues/72) |
| `retry` | `Policy`, `Default`, `Record`, `Snapshot`, `WithRecord`, `GaveUp`, `MarkUnary`, `WithHeaderTimeout`, `ErrNoResponse`, `MetadataKey` | core-agent | partly experimental | What stays public depends on [Q7](https://github.com/go-steer/core-models/issues/75). `Record`/`Snapshot` and the give-up reason are the likely stable core |
| `toolwire` | `Entry`, `EntryFor`, `Verify`, `Summary`, `Wire` | neither (tests and dialects) | internal candidate | The tool-schema invariant the adapters are held to. No product imports it |
| `pricing` | `Catalog`, `NewCatalog`, `Rates`, `LongContext`, `ModelRates`, `Options`, `Refresh`, built-in tables, `IntroductoryRates`, the file layer (`UserFile`, `ProjectFile`, `LoadUserFile`, `SaveUserFile`, `ProjectFileName`) | mast, core-agent | stable core, file layer to decide | 32 exported names. The file layer encodes where a product keeps `pricing.json`, which is close to product policy (house rule #12). Data versioning is [#71](https://github.com/go-steer/core-models/issues/71) |
| `kvmetrics` | `Sampler`, `New`, `FromProfile`, `Window`, `Between`, `Reading`, `Run`, `Options`, `ErrNoMetrics`, `ErrStopped` | mast | experimental | One consumer, introduced in v0.10.0 |
| `dialect/openaichat` | `Client`, `New`, `Options`, `ModelOptions`, `APIError`, metadata keys | mast | stable | mast also uses it directly; most callers reach it through `Open` |
| `dialect/openairesp` | `Client`, `New`, `Options`, `ModelOptions`, `APIError`, metadata keys | neither | stable | Reached through `Open` |
| `dialect/anthropic` | `Client`, `New`, `Options`, `CacheOptions`, `BuiltinTools`, `APIError`, TTL and version constants | mast, core-agent | stable | Both products use cache options directly |
| `dialect/gemini` | `Client`, `New`, `Options`, `BuiltinTools`, `ContextCache` and its hook types, `GroundingEvidence`, `IsTransient`, `IsBareInvalidArgument`, `IsBareInvalidArgumentBody`, `ErrEmptyResponse` | core-agent | stable, except `ContextCache` (experimental) | The cache hooks were introduced in v0.8.0 and serve one product's cache design |
| `dialect/gemini/vertexcache` | `Manager`, `NewManager`, `CachesClient`, `Options`, `Status`, `Gone`, `IsBelowCacheMinimum` | core-agent | experimental | One consumer, closely tied to Vertex's caching API |
| `adkv2` (module) | `Wrap`, `FromADK`, `Request`, `Response`, `ADKRequest`, `ADKResponse`, `GroundingProjection` | mast, core-agent | stable | The ADK v2 shim; it exposes ADK types by design |

## genai and ADK types in exported signatures

These signatures tie core-models' compatibility to genai's. That coupling is
accepted (design D7, decided in
[#69](https://github.com/go-steer/core-models/issues/69)): a breaking change to
one of these types is a core-models major. The
[modules page](/reference/modules/#genai-types-in-the-contract)
has the complete list; the main ones are:
- **`llm.Request`:** `[]*genai.Content` and `*genai.GenerateContentConfig`.
- **`llm.Response`:** `*genai.Content`, `CitationMetadata`, `GroundingMetadata`,
  `GenerateContentResponseUsageMetadata`, `LogprobsResult`, `Transcription` and
  `genai.FinishReason`.
- **`toolwire.EntryFor`:** `*genai.FunctionDeclaration`.
- **`gemini.GroundingEvidence`:** `*genai.GroundingMetadata`.
- **`gemini.ContextCacheInitFn`:** `*genai.Content` and `[]*genai.Tool`.

`adkv2`'s ADK v2 types are its purpose. The core module imports no ADK package,
per house rule #8.

## How errors surface today

Tracked in [#70](https://github.com/go-steer/core-models/issues/70).

| Dialect | Type | How a product classifies it |
|---|---|---|
| `openaichat`, `openairesp` | `*APIError`, with public fields | `HTTPStatus()`, `Type`, `Code` |
| `anthropic` | `*APIError`, opaque, wrapping the SDK error | `HTTPStatus()`, `MidStream()`, `errors.As` to the SDK error |
| `gemini` | genai's own errors, unwrapped | `gemini.IsTransient`, `gemini.IsBareInvalidArgument`, `vertexcache.Gone`, `vertexcache.IsBelowCacheMinimum` |

Some errors carry no type:
- Refusals of unsupported settings or parts are plain errors. A product can
  only read their text.
- The sentinels are `llm.ErrEmptyResponse`, `gemini.ErrEmptyResponse` (which
  wraps the first), `retry.ErrNoResponse`, `auth.ErrNoADC`,
  `kvmetrics.ErrNoMetrics` and `kvmetrics.ErrStopped`.

Both products classify some provider errors by their text:
- core-agent's attach layer matches `retry.ErrNoResponse`'s message, so that it
  stays free of provider dependencies.
- mast's attach layer matches rate-limit and quota substrings.
