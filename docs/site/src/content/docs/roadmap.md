---
title: Roadmap
description: The phases core-models is built in, what each delivers, and how each one is judged done.
---

Each phase names what it delivers and how it is judged done.

| Phase | Delivers | Status |
|---|---|---|
| **L0** | Repository, CI, the `llm` contract, the `usage` record, the `adkv1` and `adkv2` adapter modules | **done**. `adkv1` is deprecated since v0.6.6: both products are on ADK v2 and `adkv2` |
| **L1** | OpenAI Chat Completions, serving the built-in profiles for Vertex AI partner models, vLLM, SGLang, Ollama and any OpenAI-compatible endpoint; mast adopts it | **done**: the adapter, `coremodels.Open` and a recorded corpus (v0.1.0); mast adopted it (mast #510); GLM 5.2 holds tool-calling parity with Claude on mast's judged corpus over three runs; vLLM on GKE runs end to end (`deploy/gke-vllm`). See [tested models](/reference/tested-models/) |
| **L2** | OpenAI Responses; OpenAI and xAI profiles | **built, live test owed**: `dialect/openairesp` and the `openai` and `xai` profiles, with a conformance corpus recorded from vLLM's `/v1/responses`. Still owed: the live tool loop against OpenAI and xAI (no key yet) |
| **L3** | KV-cache statistics from self-hosted servers; core-agent adopts L1–L3 | **library side done**: core-agent adopted provider profiles (core-agent #1294), and [`kvmetrics`](/concepts/kv-cache-metrics/) samples vLLM (verified live on 0.31), SGLang and llama.cpp metrics endpoints. Still owed for the exit criterion: mast's `/usage` showing a session's fleet-level hit rate from it |
| **L4** | Anthropic adapter moves here, with the best of both products' versions | **done**: `dialect/anthropic` and the `anthropic` and `anthropic-vertex` profiles (v0.6.0); mast (mast #521) and core-agent (core-agent #1296) both run on it, with judged-eval and live parity against their previous adapters. Claude on the first-party API is offline-tested only |
| **L5** | Gemini adapter and Vertex context caching move here | **done**: `dialect/gemini`, `dialect/gemini/vertexcache` and the `gemini`, `vertex` and `vertex-express` profiles (v0.6.0–v0.6.3); mast and core-agent both run on it, with judged-eval and live parity, including web search and the Vertex context cache |
| **L6** | One price catalog for both products | planned |
| **Later** | Claude on Bedrock, Azure OpenAI, more built-in profiles | planned |

The [design document](https://github.com/go-steer/core-models/blob/main/docs/design.md)
has the exit criterion for each phase.
