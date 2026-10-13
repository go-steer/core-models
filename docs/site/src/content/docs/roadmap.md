---
title: Roadmap
description: The phases core-models is built in, what each delivers, and how each one is judged done.
---

Each phase names what it delivers and how it is judged done.

| Phase | Delivers | Status |
|---|---|---|
| **L0** | Repository, CI, the `llm` contract, the `usage` record, the `adkv1` and `adkv2` adapter modules | **done**. `adkv1` is deprecated since v0.6.6: both products are on ADK v2 and `adkv2` |
| **L1** | OpenAI Chat Completions, serving the built-in profiles for Vertex AI partner models, vLLM, SGLang, Ollama and any OpenAI-compatible endpoint; mast adopts it | **done**: the adapter, `coremodels.Open` and a recorded corpus (v0.1.0); mast adopted it (mast #510); GLM 5.2 holds tool-calling parity with Claude on mast's judged corpus over three runs; vLLM on GKE runs end to end (`deploy/gke-vllm`). See [tested models](/reference/tested-models/) |
| **L2** | OpenAI Responses; OpenAI and xAI profiles | **done**: `dialect/openairesp` and the `openai` and `xai` profiles. Recorded live from OpenAI (`gpt-5-mini`, including the encrypted-reasoning round trip) and xAI (`grok-4.20-0309-reasoning` over Chat Completions and Responses), plus Gemma 4 on our vLLM's `/v1/responses` |
| **L3** | KV-cache statistics from self-hosted servers; core-agent adopts L1–L3 | **done**: core-agent adopted provider profiles (core-agent #1294); [`kvmetrics`](/concepts/kv-cache-metrics/) samples vLLM (verified live on 0.31), SGLang and llama.cpp metrics endpoints; mast's `/usage` reports a fleet-level prefix-cache window per turn and per session, and its judged eval board per incident (mast #543), live against our vLLM at 85–95% hit rates |
| **L4** | Anthropic adapter moves here, with the best of both products' versions | **done**: `dialect/anthropic` and the `anthropic` and `anthropic-vertex` profiles (v0.6.0); mast (mast #521) and core-agent (core-agent #1296) both run on it, with judged-eval and live parity against their previous adapters. Claude on the first-party API is offline-tested only |
| **L5** | Gemini adapter and Vertex context caching move here | **done**: `dialect/gemini`, `dialect/gemini/vertexcache` and the `gemini`, `vertex` and `vertex-express` profiles (v0.6.0–v0.6.3); mast and core-agent both run on it, with judged-eval and live parity, including web search and the Vertex context cache |
| **L6** | One price catalog for both products | **library done, mast adopted**: [`pricing`](/concepts/pricing/) (v0.11.0) merges both products' catalogs, with backend-keyed built-in rows, long-context rates, a profile-declared layer and the regen tool. mast prices from it (mast #544), and its own introductory-rate check and reviewed pin bumps keep the safeguards. Still owed: core-agent adopting it |
| **Later** | Claude on Bedrock, Azure OpenAI, more built-in profiles | planned |

The [design document](https://github.com/go-steer/core-models/blob/main/docs/design.md)
has the exit criterion for each phase.
