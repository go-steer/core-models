---
title: Providers
description: Which providers core-models will support, in what order, and what has to be true before any of them is called supported.
sidebar:
  order: 4
---

core-models has **three adapters: OpenAI Chat Completions, Anthropic
Messages and Gemini**. Chat Completions has been run against Ollama, vLLM
and Vertex AI's partner-model endpoint, and Messages against Claude on
Vertex AI. mast and core-agent still reach Gemini and Claude through their
own code until each switches to these adapters.

## Three wire formats, not one adapter per vendor

Most models are reachable through one of three API shapes:

| Wire format | Reaches | Status |
|---|---|---|
| OpenAI Chat Completions | Vertex AI partner models, xAI, vLLM, SGLang, Ollama, llama.cpp, NVIDIA NIM, Groq, Together, Fireworks, DeepSeek, Mistral, any LiteLLM or OpenRouter endpoint | **built** (`dialect/openaichat`) |
| OpenAI Responses | OpenAI, xAI | planned |
| Anthropic Messages | Claude on Anthropic and Vertex AI; Bedrock later | **here** (`dialect/anthropic`), not yet adopted by the products |
| genai | Gemini on the Developer API and Vertex AI | **here** (`dialect/gemini`), not yet adopted by the products |

Chat Completions comes first because that one adapter reaches the most
models, both managed and self-hosted.

## Claude: one adapter from two

mast and core-agent each had a Claude adapter, copied from one source and
changed in different directions since. `dialect/anthropic` keeps mast's
shape (option structs, usage in `usage.Detail`) and its per-model thinking
request, and takes core-agent's prompt caching:

- **Prompt caching is on by default** when a profile is opened: a
  breakpoint on the system block, which caches the tool schemas with it,
  and rolling breakpoints over the conversation tail, so a growing
  transcript is re-read at the cache rate instead of re-billed every turn.
  `coremodels.Options.PromptCache` changes the policy, including the
  one-hour TTL, and `callctx.WithoutPromptCache` turns it off for a side
  call whose prefix will never be sent again.
- **Cache writes are recorded**, the one-hour share separately, so a meter
  bills them at the write rate rather than as fresh input.
- **Thinking follows the model's generation.** Models from
  `claude-opus-4-7` on reject the older budget-carrying thinking request,
  and the 4-5 generation rejects the newer adaptive one; the adapter sends
  each the one it accepts.
- **Retries happen in the HTTP client**, as for every dialect. The SDK's
  own retries are off.
- **Server-side tools are off** unless named in `coremodels.Options.Builtins`.

Claude on Vertex AI uses core-models' own Google credentials rather than
the SDK's Vertex package, so the library takes no OAuth2 dependency.

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

Ten server and model pairs are recorded and replayed offline on every pull request, Claude on Vertex AI among them. Five
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
