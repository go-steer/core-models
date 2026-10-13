---
title: KV-cache metrics from self-hosted servers
description: The opt-in kvmetrics sampler — what it reads from a vLLM, SGLang or llama.cpp metrics endpoint, what a window means, and what it never claims.
sidebar:
  order: 5
---

Token counts come from every response, on every server, through the
[usage record](/concepts/usage-record/). What a response can't tell you is
how the server's KV cache behaved: how much of the prompt it found already
cached across all its traffic, and how full the cache got. A self-hosted
server publishes that on its Prometheus endpoint, and package `kvmetrics`
reads it.

It is **opt-in**, and it provides facts, not policy. The library reads the
server; each product decides when. mast samples at session start and end,
and core-agent per `/stats`. A library embed with no metrics endpoint never
builds a sampler, and an operator who already scrapes the server with
Prometheus can ignore it.

## Fleet-level, time-correlated

A server's counters cover every request it served, from every client. So a
window says what the **server** did while a session ran, not what the
session did. Every `Window` carries `Fleet: true` so that fact travels with
the numbers. A window is never attributed to a request or a specialist, and
never enters the usage record or a cost figure.

## Using it

A profile names the endpoint and the server's metric vocabulary:

```json
{"name": "lab", "extends": "vllm", "base_url": "http://vllm:8000/v1",
 "metrics_url": "http://vllm:8000/metrics"}
```

The `vllm`, `sglang` and `ollama` built-ins set `metrics` (`vllm`, `sglang`,
`none`), and a profile that extends one inherits it. A profile on another
built-in sets it itself, for example `"metrics": "llamacpp"` beside
`"extends": "openai-compatible"`.

```go
r, _ := profile.Resolve(ctx, p, profile.Options{})
s, _ := kvmetrics.FromProfile(r, "")        // "" = the whole server; or one model id
run, _ := s.Start(ctx, 15*time.Second)      // 0 = no polling between the endpoints
// … the session runs …
w, _ := run.Stop(ctx)
```

`FromProfile` uses the profile's credential too, so a bearer-protected
endpoint gets the same token as the API. A product that schedules its own
scrapes calls `Snapshot` and combines readings with `Between`.

## What a window holds

| Field | Meaning |
|---|---|
| `Queries`, `Hits` | Prefix-cache tokens looked up and found, increased over the window |
| `HitRate` | `Hits / Queries`, or the server's own rate gauge when it has no counters |
| `PeakKVUsage` | Highest KV-cache utilization (0–1) at the endpoints or any poll between |
| `Reset` | A counter went backwards: the server restarted mid-window |
| `Fleet` | Always `true` |

Every measure is a pointer. A metric the server doesn't expose is `nil`,
"not reported", never `0`.

Polling (`Start` with an interval) is what makes `PeakKVUsage` meaningful:
without it the peak is the higher of the two endpoints and can miss a spike
between them. A failed poll is skipped, and `Run.PollErr` reports it.

If the server restarts mid-window, its counters drop to zero. The increase
is then summed per segment, the way Prometheus's `increase()` does, and
`Reset` is set. Whatever the server served between the last reading before
the restart and the restart itself isn't counted.

## Servers

| Server | Read | Verified |
|---|---|---|
| vLLM (V1) | `vllm:prefix_cache_queries_total`, `vllm:prefix_cache_hits_total`, `vllm:kv_cache_usage_perc`; earlier V1 builds' `vllm:gpu_*` names as fallbacks | Live, on vLLM 0.31 |
| SGLang | `sglang:prompt_tokens_total` and `sglang:cached_tokens_total` as queries and hits where the build has them, otherwise the `sglang:cache_hit_rate` gauge; `sglang:token_usage`. Either `sglang:` or `sglang_` prefix | From SGLang's documentation |
| llama.cpp | `llamacpp:kv_cache_usage_ratio`. It has no prefix-cache hit counters, so `Queries` and `Hits` stay nil | From llama.cpp's documentation |
| Ollama | Nothing: it exposes no KV-cache statistics. `New` returns `ErrNoMetrics` rather than a sampler that would report an idle cache | — |

Counters are summed across a server's engines. Utilization takes the
busiest engine, because an average would hide a full one. When a server
serves several models, the default reading sums them; pass a model id to
read only that model's series (vLLM and SGLang label them `model_name`).
