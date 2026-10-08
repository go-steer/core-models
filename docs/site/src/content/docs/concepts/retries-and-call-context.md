---
title: Retries and per-call context
description: Why core-models retries at the HTTP layer, what it retries and for how long, and the context markers a caller uses to change one call.
sidebar:
  order: 3
---

## Retries happen at the HTTP layer

A rate-limited provider tells you how long to wait in a response header:
`retry-after-ms`, `retry-after`, or `x-should-retry`. A retry wrapper
around the model interface usually can't see those headers. The Gemini SDK
discards the response before the error reaches it, and each vendor SDK
retries by its own rules: the Anthropic SDK retries twice, the Gemini SDK
never. The same agent then got different resilience depending on which
model it named.

So core-models adapters turn their SDK's own retries off and install
`retry.Policy.Transport` on the HTTP client. One policy and one header rule
apply to every provider. A product's own retry wrapper still runs, and now
sees only what this layer gave up on.

## What is retried, and for how long

| | |
|---|---|
| **Retried** | Status 408, 429, 500, 502, 503 or 504; a connection that failed before a response arrived; any status the server marks `x-should-retry: true` |
| **Not retried** | Any status marked `x-should-retry: false`; other 4xx; a canceled context; a request body that can't be sent again; a stream that breaks after it started, since replaying it would deliver tokens twice |
| **Wait** | `retry-after-ms` if present, otherwise `retry-after` as seconds or an HTTP date, otherwise exponential backoff with jitter |
| **Too long** | If the server asks for more than `MaxHeaderDelay` (default one minute), the response is handed back without waiting. Retrying sooner than asked just earns another 429 |
| **Default budget** | Two retries, starting at one second |

What happened is recorded on a per-call `retry.Record`. An adapter stamps
it onto the response, so a retry shows up in the transcript and not only in
a log.

## Per-call context markers

One model serves an agent's main loop and its side calls alike, such as an
approver, a session title or a summary. Anything that differs per call
therefore travels on the call's context, in package `callctx`:

| Marker | Effect |
|---|---|
| `AsSideCall(ctx, name)` | Names the call as a side call. Retry and logging use the name, and a side call never counts as evidence from its session's earlier successes |
| `WithoutBuiltins(ctx)` | The request carries exactly the caller's tools: no provider-injected search, URL context or code execution |
| `WithoutPromptCache(ctx)` | No prompt-cache markers. A side call whose prefix won't be sent again would pay the cache-write premium for nothing |
| `WithPriorSuccess(ctx, rec)` | Records that this session has already been served once. Some ambiguous rejections are worth one retry only if so |

Every marker can only take something away from a request. A marker an
adapter doesn't honor is a missed optimization, never a wrong request.
