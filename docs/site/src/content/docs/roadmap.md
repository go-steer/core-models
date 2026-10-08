---
title: Roadmap
description: The phases core-models is built in, what each delivers, and how each one is judged done.
---

Each phase names what it delivers and how it is judged done.

| Phase | Delivers | Status |
|---|---|---|
| **L0** | Repository, CI, the `llm` contract, the `usage` record, the `adkv1` and `adkv2` adapter modules | **in progress**: contract, usage record, adapter modules, call context and HTTP-layer retry done; profiles and credentials next |
| **L1** | OpenAI Chat Completions; profiles for Vertex AI partner models, vLLM, SGLang, Ollama and any OpenAI-compatible endpoint; mast adopts it | planned |
| **L2** | OpenAI Responses; OpenAI and xAI profiles | planned |
| **L3** | KV-cache statistics from self-hosted servers; core-agent adopts L1–L3 | planned |
| **L4** | Anthropic adapter moves here, with the best of both products' versions | planned |
| **L5** | Gemini adapter and Vertex context caching move here | planned |
| **L6** | One price catalog for both products | planned |
| **Later** | Claude on Bedrock, Azure OpenAI, more built-in profiles | planned |

The [design document](https://github.com/go-steer/core-models/blob/main/docs/design.md)
has the exit criterion for each phase.
