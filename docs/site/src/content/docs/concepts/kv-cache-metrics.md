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
s, _ := kvmetrics.FromProfile(r, kvmetrics.Options{}) // Options.Model narrows to one model
run, _ := s.Start(ctx, 15*time.Second)      // 0 = no polling between the endpoints
// … the session runs …
w, _ := run.Stop(ctx)
```

`FromProfile` sends the profile's credential only to a metrics endpoint
with the **same origin** (scheme, host and port) as the profile's
`base_url`, the way package `auth` sends it to the API (a Bearer header,
for an API key too), and never a Google ADC token over plain `http`. A
metrics endpoint on another host is scraped without credentials. One that
needs a different credential takes your own `Options.HTTPClient`, to which
nothing is added.

A product that schedules its own scrapes calls `Snapshot` and combines
readings with `Between`, which folds them exactly as a `Run` does.

## What a window holds

| Field | Meaning |
|---|---|
| `Queries`, `Hits` | Prefix-cache tokens looked up and found, increased over the window, computed per series and summed |
| `HitRate` | `Hits / Queries` over the window; nil when either is missing |
| `HitRateGauge` | The server's own hit-rate gauge at the last reading that had one: instantaneous, for servers without counters, and kept apart from `HitRate` |
| `PeakKVUsage` | Highest KV-cache utilization (0–1) at the endpoints or any poll between |
| `Reset` | Some series' counter went backwards: an engine restarted mid-window |
| `EndFailed` | The closing scrape failed, so the window ends at the last successful poll |
| `Fleet` | Always `true` |

Every measure is a pointer. A metric the server doesn't expose is `nil`,
"not reported", never `0`.

Polling (`Start` with an interval) is what makes `PeakKVUsage` meaningful:
without it the peak is the higher of the two endpoints and can miss a spike
between them. A failed poll is skipped, and `Run.PollErr` reports it. A run
folds readings as they arrive, so its memory doesn't grow with the session.
If the closing scrape fails, `Stop` still returns the window up to the last
successful reading, with `EndFailed` set, alongside the error.

Counter increases are computed **per series** (metric name and full label
set), then summed, the way Prometheus's `increase()` works. So one engine
restarting while others keep growing is counted correctly, and `Reset` is
set. A series that appears mid-window counts from its first reading; one
that disappears contributes up to its last. Whatever an engine served
between the last reading before its restart and the restart itself isn't
counted. A scrape over 16 MiB is refused rather than read in part.

## Servers

| Server | Read | Verified |
|---|---|---|
| vLLM (V1) | `vllm:prefix_cache_queries_total`, `vllm:prefix_cache_hits_total`, `vllm:kv_cache_usage_perc`; earlier V1 builds' `vllm:gpu_*` names as fallbacks | Live, on vLLM 0.31 |
| SGLang | `sglang:prompt_tokens_total` and `sglang:cached_tokens_total` as queries and hits where the build has them, otherwise the `sglang:cache_hit_rate` gauge; `sglang:token_usage`. Either `sglang:` or `sglang_` prefix | From SGLang's documentation |
| llama.cpp | `llamacpp:kv_cache_usage_ratio`. It has no prefix-cache hit counters, so `Queries` and `Hits` stay nil | From llama.cpp's documentation |
| Ollama | Nothing: it exposes no KV-cache statistics. `New` returns `ErrNoMetrics` rather than a sampler that would report an idle cache | — |

Counters are summed across a server's engines. Utilization takes the
busiest engine, because an average would hide a full one; SGLang's own
hit-rate gauge is averaged across engines, because a maximum would
overstate it and no series exposes a weight. When a server serves several
models, the default reading sums them; set `Options.Model` to read only
one model's series (vLLM and SGLang label them `model_name`). A model the
endpoint has no series for is an error naming the models it does have,
not a reading of nils that would look like "not reported".
