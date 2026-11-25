---
title: Modules and versions
description: The Go modules core-models publishes, their import paths, how they are tagged, and the dependency floors they impose.
---

## Import paths

| Module | Import |
|---|---|
| Core | `github.com/go-steer/core-models` (the root package `coremodels`, plus `llm`, `usage`, `callctx`, `retry`, `auth`, `profile`, `toolwire`, `kvmetrics`, `pricing`, `dialect/openaichat`, `dialect/openairesp`, `dialect/anthropic`, `dialect/gemini` and `dialect/gemini/vertexcache`) |
| ADK v2 adapter | `github.com/go-steer/core-models/adkv2` |

A consumer requires the core module and the adapter. The ADK v1 adapter,
`adkv1`, was deprecated in v0.6.6 (its last release) once both products
moved to ADK v2, and has since been deleted; its tags stay fetchable from
the module proxy.

## Tags

Each module is tagged separately, Go's convention for nested modules:
- `v0.x.y` for the core;
- `adkv2/v0.x.y` for the adapter.

core-models is pre-1.0 and its API may change between minor versions.

## genai types in the contract

`llm` mirrors ADK's model types, so genai types are part of core-models'
public API. That is a settled decision (design D7,
[#69](https://github.com/go-steer/core-models/issues/69)): a genai major, or a
breaking change to one of these types or a type they are built from, is a
core-models major, and before 1.0 a minor with an **API Change** entry. The
dependency floors below bound the exposure: a genai raise is picked up in
core-agent's bump PR, with its tests run on it.

The types, by the package whose exported API carries them:

| Package | genai types | Where |
|---|---|---|
| `llm` | `Content`, `GenerateContentConfig` | `Request` |
| `llm` | `Content`, `CitationMetadata`, `FinishReason`, `GenerateContentResponseUsageMetadata`, `GroundingMetadata`, `LogprobsResult`, `Transcription` | `Response` |
| `toolwire` | `FunctionDeclaration` | `Entry`, `EntryFor` |
| `dialect/gemini` | `Backend` | `Options` |
| `dialect/gemini` | `Caches` | `Client.Caches` |
| `dialect/gemini` | `Content`, `Tool` | `ContextCacheInitFn` |
| `dialect/gemini` | `GroundingMetadata` | `GroundingEvidence` |
| `dialect/gemini/vertexcache` | `CachedContent`, `CreateCachedContentConfig`, `UpdateCachedContentConfig`, `DeleteCachedContentConfig`, `DeleteCachedContentResponse` | `CachesClient` |
| `dialect/gemini/vertexcache` | `Content`, `Tool` | `Manager.Init` |

No other core package exposes a genai type. `adkv2` exposes ADK v2 types,
which is its purpose; the core module imports no ADK package.

## Dependency floors

A library's requirements become the minimum versions its consumers get. mast
is no longer developed, so the consumer a floor moves is core-agent (and
products built on it). The core module tracks the latest stable releases of
its SDKs, and core-agent picks each raise up in its own bump PR, with its
tests run on the new version.

| Module | Floor | Since | Why |
|---|---|---|---|
| `google.golang.org/genai` | v1.79.1 | 2026-11-25 | Latest stable. Adds `ThinkingLevelXhigh` (Gemini 4 Neon and Hydrogen accept `XHIGH`) and the `MALFORMED_RESPONSE` finish reason. Raised from v1.57.0 |
| `github.com/anthropics/anthropic-sdk-go` | v1.84.0 | 2026-11-25 | Latest stable v1. v1.76.0 is the first with `thinking.type: between_tools`, Claude Sonnet 5.5's way to turn thinking off. Raised from v1.43.0 |

genai v1.78 moved the Gemini Developer API's default version from `v1beta` to
`v1` (Vertex AI stays on `v1beta1`). No recorded exchange covers the
Developer API, so `dialect/gemini` stays on `v1beta` until `v1` is verified
live; setting `GOOGLE_GENAI_API_VERSION` chooses another version.

Google credentials come from `cloud.google.com/go/auth`, at the version
genai already requires, so the core adds no module for them. Claude on
Vertex AI uses those credentials too, rather than the SDK's `vertex`
package, which would bring in `golang.org/x/oauth2` and
`google.golang.org/api`.

For the same reason the `oauth2_client_credentials` auth kind does its
token exchange itself (one form POST, cached on `expires_in`) rather than
through `golang.org/x/oauth2/clientcredentials`, which would add a module,
and so a floor, for every consumer.

Exceptions are made for security fixes:

| Floor | Why |
|---|---|
| `google.golang.org/grpc` v1.83.1 | GO-2026-6348. Both products were already at or above it |
| `golang.org/x/net` v0.60.0 | GO-2026-6611, -6612 and -6617 (HTTP/2). This raised consumers' floor to the fixed version |
| `go.opentelemetry.io/otel/trace` v1.47.0 | GO-2026-6974 (attribute length limit bypass), reached through genai's credentials transport. Raised from v1.44.0 with `otel` and `otel/metric` |

Each `go.mod` also carries `toolchain go1.26.11` for the standard library's
fixes to the HTTP/2, `crypto/x509` and `net/http` (GO-2026-6859) issues. A
`toolchain` line governs only this repository's own builds and CI; it is not
imposed on consumers. They need Go 1.26.11 or later in their own builds to be
clear of those issues.
