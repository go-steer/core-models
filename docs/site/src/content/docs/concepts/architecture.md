---
title: Architecture
description: The provider contract, the module layout, the two ADK adapter modules, and the line between the library and the products that use it.
sidebar:
  order: 1
---

## One contract, mirrored from ADK

Every adapter implements `llm.LLM`: a model you send one request to and
get back one complete response, or a stream of partial responses followed
by a complete one. Its request and response types copy ADK's `model`
package field for field. The only exception is ADK's tool map, which no
adapter needs: the tool declarations a provider sends travel in the
request's genai config.

Mirroring rather than importing ADK is deliberate. Each ADK major's model
interface has the same shape but is a different Go type, so a library that
imported one major could not serve a consumer on another. When core-agent
was on ADK v1 and mast on ADK v2, that is what let one core serve both.

## Modules

| Module | Depends on | Used by |
|---|---|---|
| `github.com/go-steer/core-models` | genai and the vendor SDKs, **never ADK** | everything below |
| `github.com/go-steer/core-models/adkv2` | the core, `google.golang.org/adk/v2` | mast and core-agent. `Wrap` adapts an `llm.LLM` to ADK, and `GroundingProjection` turns Gemini's Google Search grounding into session events |

The adapter module converts by copying fields. It has two functions:
- `Wrap` turns a core-models model into an ADK `model.LLM`.
- `FromADK` goes the other way, so a decorator written against core-models
  can sit in front of a model ADK built.

Their tests compare the mirror against ADK's own types with reflection,
so an ADK upgrade that adds a field fails the build instead of losing the
field silently.

A presubmit protects this layout: **core module imports no ADK** fails if
any package in the core module reaches an ADK major, directly or through a
dependency. A future ADK major gets its own adapter module beside `adkv2`.

## What lives here, and what doesn't

| In core-models | In each product |
|---|---|
| Provider adapters, one per wire format | Which model each tier gets |
| Provider profiles: endpoint, credentials, capabilities | Where operators write profiles, and CLI flags |
| The [usage record](/concepts/usage-record/) | Budgets, meters and usage trackers |
| Retry at the HTTP layer, where rate-limit headers are still visible | What to do after the library gives up |
| The price catalog (later) | Price file locations and refresh policy |

The [full design](https://github.com/go-steer/core-models/blob/main/docs/design.md)
covers the parts not built yet.
