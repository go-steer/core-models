---
title: Providers
description: Which providers core-models will support, in what order, and what has to be true before any of them is called supported.
sidebar:
  order: 4
---

core-models has **three adapters: OpenAI Chat Completions, Anthropic
Messages and Gemini**. Chat Completions has been run against Ollama, vLLM
and Vertex AI's partner-model endpoint, and Messages against Claude on
Vertex AI. mast and core-agent both reach Gemini and Claude through these
adapters.

## Three wire formats, not one adapter per vendor

Most models are reachable through one of three API shapes:

| Wire format | Reaches | Status |
|---|---|---|
| OpenAI Chat Completions | Vertex AI partner models, xAI, vLLM, SGLang, Ollama, llama.cpp, NVIDIA NIM, Groq, Together, Fireworks, DeepSeek, Mistral, any LiteLLM or OpenRouter endpoint; SAP AI Core's orchestration service through its envelope | **built** (`dialect/openaichat`); SAP AI Core not yet validated against a real tenant |
| OpenAI Responses | OpenAI, xAI (opt-in), and vLLM's `/v1/responses` | **built** (`dialect/openairesp`); not yet live-tested against OpenAI or xAI |
| Anthropic Messages | Claude on Anthropic and Vertex AI; Bedrock later | **built** (`dialect/anthropic`); mast and core-agent run on it |
| genai | Gemini on the Developer API and Vertex AI | **built** (`dialect/gemini`); mast and core-agent run on it |

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

## Gemini: implicit and explicit caches

Gemini discounts a repeated prompt prefix two ways. **Implicit caching**
needs nothing from the caller: the response reports the hit as
`CachedContentTokenCount`, `usage.Detail.CacheReadTokens` carries it, and
the price catalog bills it at the cached-input rate. **Explicit caching**
(`dialect/gemini/vertexcache`) creates a named cache of the system
instruction and tools on Vertex AI and refreshes it before its TTL runs out.

Not every model takes an explicit cache. Gemini 4 Argon answers a cache
create with `does not support explicit context caching`; the manager reads
that with `vertexcache.IsCachingUnsupported`, logs it once and runs
uncached, without the retry schedule it keeps for failures time can fix.
Argon's implicit cache still works: a live call with a repeated 9,619-token
prompt read 7,565 tokens from it and cost $0.0050 instead of $0.0194.

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

## Behind an OAuth2 gateway

A model behind an authenticating gateway (Apigee, Azure API Management
with Entra ID, Kong, Okta, SAP AI Core) usually takes a short-lived token
from an OAuth2 client-credentials exchange rather than a static key. The
`oauth2_client_credentials` auth kind does that exchange from
configuration alone:

```json
{
  "name": "gateway",
  "extends": "openai-compatible",
  "base_url": "https://models.example.com/v1",
  "auth": {
    "kind": "oauth2_client_credentials",
    "token_url": "${GATEWAY_TOKEN_URL}",
    "client_id_env": "GATEWAY_CLIENT_ID",
    "client_secret_env": "GATEWAY_CLIENT_SECRET",
    "scopes": ["models.invoke"]
  }
}
```

The first token is fetched when the profile is opened, so a wrong secret
or an unreachable token endpoint fails then, naming the variables and the
endpoint's host. The token is cached and refreshed in the background
before it expires, so a session can run longer than any one token and a
slow token endpoint delays nothing while the current token is good. If the
gateway answers 401 to a token that is not brand new (revoked early), the
request is sent once more with a new token. A refused client secret is
reported, not retried. The client secret and the tokens never appear in an
error or a log line. The gateway's own metrics endpoint gets the token
only over https.

## SAP AI Core

SAP's Generative AI Hub is the gateway some organizations require model
traffic to go through. Its orchestration service serves every model SAP
offers from one deployment, and speaks Chat Completions inside SAP's own
envelope. The built-in `sap-ai-core` profile reaches it with the
credentials SAP users already have:

```bash
export AICORE_SERVICE_KEY="$(cat sap-service-key.json)"   # or AICORE_AUTH_URL, _CLIENT_ID, _CLIENT_SECRET, _BASE_URL
export AICORE_RESOURCE_GROUP=team-a                       # optional; "default" when unset
```

Opening the profile fetches a token and finds the newest running
orchestration deployment in the resource group, so a missing deployment
fails at startup. Models are SAP's names for them, such as
`anthropic--claude-4.5-sonnet` or `gpt-5-mini`. Orchestration's own modules
(filtering, masking, grounding, translation) are configured by you, through
`extra_body.modules`; the [profiles reference](/reference/profiles/#sap-ai-core)
has the details and the limits.

:::caution[Not yet validated against a real tenant]
The envelope follows SAP's Orchestration v2 API specification and SAP's
Cloud SDK for AI, and passes the shared conformance scenarios against a
fake tenant. Streaming and tool calling through orchestration have not yet
been checked against SAP itself. Until a corpus is recorded from a real
tenant ([#83](https://github.com/go-steer/core-models/issues/83)), treat it
as an unvalidated profile.
:::

## What "supported" will mean

A provider is called supported only when:
- its tool calls round-trip with their real schemas;
- its usage is measured;
- it is priced, or explicitly unpriced;
- it passes a recorded test suite offline;
- it passes a live tool-calling run at parity with Claude.

Until then it ships as an unvalidated profile template, if at all.
