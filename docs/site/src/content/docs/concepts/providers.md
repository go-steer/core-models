---
title: Providers
description: Which providers core-models will support, in what order, and what has to be true before any of them is called supported.
sidebar:
  order: 4
---

core-models has **one adapter so far: OpenAI Chat Completions**. It has
been run against a local Ollama server and Vertex AI's partner-model
endpoint. Gemini and Claude keep working in mast and core-agent through
their existing code until they move here.

## Three wire formats, not one adapter per vendor

Most models are reachable through one of three API shapes:

| Wire format | Reaches | Status |
|---|---|---|
| OpenAI Chat Completions | Vertex AI partner models, xAI, vLLM, SGLang, Ollama, llama.cpp, NVIDIA NIM, Groq, Together, Fireworks, DeepSeek, Mistral, any LiteLLM or OpenRouter endpoint | **built** (`dialect/openaichat`) |
| OpenAI Responses | OpenAI, xAI | planned |
| Anthropic Messages | Claude on Anthropic, Vertex AI and Bedrock | moves here from the products |
| genai | Gemini on the Developer API and Vertex AI | moves here from the products |

Chat Completions comes first because that one adapter reaches the most
models, both managed and self-hosted.

## Profiles instead of model-name prefixes

Today a model name's prefix (`gemini-`, `claude-`) picks the vendor. That
breaks down for the names new backends use: `Qwen/Qwen3-Coder-Next`, an
Azure deployment you named yourself, or the same `gpt-oss-20b` served by
both Vertex AI and your own vLLM at different prices.

core-models names a model by a **profile** plus a model id. A profile
declares:
- the endpoint;
- the credential;
- the wire format;
- what the server can do: structured output, reasoning, server-side tools.

A profile that can't be resolved fails when it is loaded, naming the
profile, rather than at the first request. Every field and the built-in
profiles are on the [profiles reference](/reference/profiles/).

## Tested against real servers

Six servers are recorded and replayed offline on every pull request. Five
Vertex AI partner models have also been through a tool-calling parity run
against Claude, on mast's 31-incident Kubernetes corpus. GLM 5.2 and Kimi
K2 Thinking match Claude on intent coverage. Llama 4 Maverick is left out
for tool use. The results, and what each test means, are on
[Tested models](/reference/tested-models/).

## Self-hosted models

Every self-hosted server reports input and output tokens, and core-models
records them. Cached-token reporting varies by server and version, so a
profile can mark it unreliable rather than show a wrong number.

KV-cache statistics come from the server's Prometheus metrics, when you
point a profile at them. They describe the whole server over a time
window, not one request, and are labelled that way.

Self-hosted models have no per-token price. Cost shows as unpriced unless
you declare a rate.

## What "supported" will mean

A provider is called supported only when:
- its tool calls round-trip with their real schemas;
- its usage is measured;
- it is priced, or explicitly unpriced;
- it passes a recorded test suite offline;
- it passes a live tool-calling run at parity with Claude.

Until then it ships as an unvalidated profile template, if at all.
