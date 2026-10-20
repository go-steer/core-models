---
title: Profiles
description: Every field of a core-models provider profile, the built-in profiles, and how extends, ${VAR} expansion and validation work.
---

A profile declares how to reach models: the endpoint, the credential, the
wire format, what the server can do, and which models it serves. Package
`profile` owns the schema. Each product decides where operators write
their profiles.

## Example

A self-hosted vLLM server, starting from the built-in `vllm` template:

```json
[
  {
    "name": "house-vllm",
    "extends": "vllm",
    "base_url": "http://vllm.infra.svc:8000/v1",
    "metrics_url": "http://vllm.infra.svc:8000/metrics",
    "usage": {"cached_tokens": "unreliable"},
    "models": [{"id": "Qwen/Qwen3-Coder-Next", "tier": "mid", "context_window": 262144}],
    "tiers": {"mid": "Qwen/Qwen3-Coder-Next"}
  }
]
```

## Fields

| Field | Meaning |
|---|---|
| `name` | What `--provider` and product config refer to. Lowercase letters, digits and `-` |
| `extends` | A built-in profile to start from. Every field set here overrides the built-in's |
| `dialect` | `openai-chat`, `openai-responses`, `anthropic` or `gemini` |
| `platform` | Where the vendor's API is hosted, when that changes the request. `vertex` routes an `anthropic` profile the way Vertex AI serves Claude: the model in the URL path, `anthropic_version` in the body. Set on `anthropic-vertex` and inherited by profiles extending it. `sap-orchestration` routes an `openai-chat` profile through SAP AI Core's orchestration service (see [SAP AI Core](#sap-ai-core)); set on `sap-ai-core`. Unset means the vendor's own API. Said explicitly because neither the credential nor the backend name settles it: a proxy in front of Vertex may take a bearer token |
| `base_url` | The API root. May contain `{param}` placeholders and `${VAR}` references. Required for the OpenAI dialects. For `anthropic` with `platform: vertex`, the Vertex AI publisher prefix (`…/publishers/anthropic/models`) |
| `params` | Values for `base_url`'s placeholders. Each may be `${VAR}` or `${VAR:-default}`, and a default may itself be a reference: `${A:-${B:-x}}` is the first of `A` and `B` that is set, else `x` |
| `auth.kind` | `api_key` or `bearer` (with `auth.env` naming the variable that holds it), `google_adc`, `oauth2_client_credentials`, or `none` |
| `auth.scopes` | OAuth scopes for `google_adc` (defaults to cloud-platform) or `oauth2_client_credentials` (none by default) |
| `auth.token_url` | `oauth2_client_credentials`: the token endpoint. May be or contain `${VAR}` and `${VAR:-default}` |
| `auth.client_id_env`, `auth.client_secret_env` | `oauth2_client_credentials`: the variables holding the client id and secret |
| `auth.audience` | `oauth2_client_credentials`: an `audience` for identity providers that require one. Optional |
| `auth.client_auth` | `oauth2_client_credentials`: `basic` (HTTP Basic, the default) or `post` (id and secret in the form body) |
| `headers` | Headers sent on every request to the model endpoint, for a gateway that needs one that isn't the credential: SAP AI Core's `AI-Resource-Group`, an API-management subscription key, a tenant or routing header, an `anthropic-beta` opt-in. Values may be `${VAR}` or `${VAR:-default}`, read at Resolve; a header whose value comes out empty isn't sent. Credentials (`Authorization`, `x-api-key`, `api-key`, `x-goog-api-key`, `x-goog-user-project`) and the headers the dialects set (`Content-Type`, `Accept`, `User-Agent`, `anthropic-version`, …) are refused. Names are case-insensitive and sent in canonical form. See [Headers](#headers) |
| `backend` | The name prices are keyed on. Defaults to `name`, except that a profile extending `vertex-maas`, `gemini` or `vertex` keeps the built-in's |
| `open_models` | Accept any model id, not only the listed ones. Set on every self-hosted template |
| `capabilities` | `response_schema`, `reasoning_echo`, `parallel_tool_calls`, `server_tools`, `streaming`, `forced_tool_choice`. Each is true, false, or unset. Unset reads as absent, except `forced_tool_choice`, which is assumed supported unless declared false |
| `usage.cached_tokens` | `unreliable` records cached tokens as not reported even when the server sends them |
| `reasoning_format` | `think_tags` for a server that puts reasoning inline as a leading `<think>…</think>` block (Ollama; vLLM or SGLang without a reasoning parser). The block becomes a reasoning part instead of answer text |
| `metrics_url` | The server's Prometheus endpoint, for the opt-in [KV-cache sampler](/concepts/kv-cache-metrics/) |
| `metrics` | Which server's metric names `metrics_url` exposes: `vllm`, `sglang`, `llamacpp` or `none`. The `vllm`, `sglang` and `ollama` built-ins set it (`ollama` as `none`), and a profile extending one inherits it |
| `extra_body` | Vendor fields added to every request, for switches the dialect doesn't model. For example, `{"chat_template_kwargs": {"enable_thinking": true}}` turns on Gemma 4's thinking on vLLM. Can't set a field the adapter sets itself (`model`, `messages`, `tools`, `temperature`, …) |
| `models` | `id`, plus an optional `tier` (`small`, `mid` or `frontier`), `context_window`, and `capabilities` and `extra_body` that override the profile's for that model only. `context_window` is the input window in tokens **as this server serves it**, which for a self-hosted model can be less than the model's own (vLLM's `--max-model-len`). Hosts read it with `Profile.ContextWindowFor` to size compaction, and fall back to their own table when it's absent |
| `models[].rates` | A declared price, in USD per million tokens: `input_per_mtok`, `output_per_mtok`, `cached_input_per_mtok`, `cache_write_per_mtok`. For a server no published catalog covers, such as your own vLLM. Products price from their catalog first and fall back to these. An input or output price is required, because declaring a model free would make a cost ceiling that never trips |
| `tiers` | The model for each tier the profile can fill |

Capabilities are per profile and refined per model, because two models
behind one server can differ: one needs its reasoning echoed back, another
rejects a forced tool call. With `forced_tool_choice: false`, a request
that forces a tool call (`required`, or one named tool) is sent as `auto`
instead, and the response carries `core_models.tool_choice_downgraded`.

`extends` merges `params`, `headers` and `tiers` key by key (`headers`
case-insensitively, and an empty value drops an inherited header). It
replaces `models` and `auth` as a whole, and overrides each capability on
its own.

## Headers

A gateway in front of a model often wants a header beside the credential.
An Azure API Management front end, for example:

```json
[
  {
    "name": "apim-vllm",
    "extends": "vllm",
    "base_url": "https://models.example.azure-api.net/vllm/v1",
    "auth": {"kind": "bearer", "env": "APIM_TOKEN"},
    "headers": {"Ocp-Apim-Subscription-Key": "${APIM_SUBSCRIPTION_KEY}", "X-Tenant": "${TENANT:-shared}"}
  }
]
```

- `Open` sends them on every attempt, retries included, in every dialect.
  A host that builds a dialect itself instead of through `Open` (for
  example `gemini.New`, to share one client with its context cache) wraps
  the `HTTPClient` it passes with `Resolved.HeaderTransport`.
- They go to the model endpoint only: not to a token endpoint, and not to
  another host a redirect points to. A `metrics_url` gets them when it is
  on the API's own scheme and host, by the same rule as the credential.
- A header the request already carries keeps its value, so a per-call
  setting wins over the profile's default.
- A value may be a secret, so keep it in the environment and reference it.
  A value read from the environment never appears in an error or a printed
  `Resolved`; one written literally into the profile is part of the
  profile, like any other field. `Resolved.HeaderNames` lists the names for
  a host to show.

## Built-in profiles

| Profile | Dialect | Endpoint | Auth | Notes |
|---|---|---|---|---|
| `anthropic` | anthropic | `https://api.anthropic.com` | API key from `ANTHROPIC_API_KEY`, sent as `x-api-key` | Any Claude id. No tiers: which model a tier means is the product's call |
| `anthropic-vertex` | anthropic | Vertex AI's Claude publisher prefix for the project and region | Google ADC | `platform: vertex`. Project from `ANTHROPIC_VERTEX_PROJECT_ID`, then `GOOGLE_CLOUD_PROJECT`. Region from `CLOUD_ML_REGION`, then `GOOGLE_CLOUD_LOCATION`, default `us-east5`; `global`, `us` and `eu` get their own hosts. Backend `anthropic-vertex`. Ids as Vertex names them, such as `claude-opus-4-5@20251101` |
| `gemini` | gemini | The Gemini Developer API | API key from `GOOGLE_API_KEY`, or `GEMINI_API_KEY` | Backend `gemini`. Server-side tools (`web_search`, `url_context`, `code_execution`) are off unless asked for |
| `vertex` | gemini | Gemini on Vertex AI for the project and region | Google ADC | Backend `vertex`. Project from `GOOGLE_CLOUD_PROJECT`. Region from `GOOGLE_CLOUD_LOCATION`, then `GOOGLE_CLOUD_REGION`, default `global` |
| `vertex-express` | gemini | Gemini on Vertex AI in express mode: an API key, no project | API key (`GOOGLE_API_KEY`) | Backend `vertex`, so it prices like `vertex`. `platform: vertex` routes the key to Vertex AI instead of the Developer API. The key has to be allowed to call the Vertex AI API |
| `openai` | openai-responses | `https://api.openai.com/v1` | API key from `OPENAI_API_KEY` | Backend `openai`. Stateless (`store: false`); a reasoning model's encrypted reasoning is carried forward between turns. Any id is accepted: when a model refuses `include`, `temperature` or `top_p`, the request is retried once without it, the refusal is remembered for that model, and the response lists what was left off (`core_models.dropped_params`) |
| `xai` | openai-chat | `https://api.x.ai/v1` | API key from `XAI_API_KEY` | Backend `xai`. To use xAI's Responses API instead, extend it with `"dialect": "openai-responses"` |
| `vertex-maas` | openai-chat | Vertex AI's OpenAI-compatible endpoint for the project and region | Google ADC | Project from `GOOGLE_CLOUD_PROJECT`. Region from `GOOGLE_CLOUD_LOCATION`, default `global`. Ids are publisher-qualified, such as `openai/gpt-oss-20b-maas`. Lists the gpt-oss models with `forced_tool_choice: false`, per Google's function-calling notes; any other id is still accepted |
| `ollama` | openai-chat | `http://localhost:11434/v1` | none | Works with nothing set. `reasoning_format: think_tags` |
| `vllm` | openai-chat | *template: set `base_url`* | none | Parallel tool calls declared |
| `sglang` | openai-chat | *template: set `base_url`* | none | Parallel tool calls declared |
| `openai-compatible` | openai-chat | *template: set `base_url`* | none | For any other OpenAI-compatible endpoint; set `auth` too |
| `sap-ai-core` | openai-chat | SAP AI Core's orchestration deployment, found at startup | OAuth2 client credentials from `AICORE_SERVICE_KEY`, or `AICORE_AUTH_URL`, `AICORE_CLIENT_ID`, `AICORE_CLIENT_SECRET` and `AICORE_BASE_URL` | `platform: sap-orchestration`. Backend `sap-ai-core`, with no prices. Any id SAP's orchestration serves, such as `anthropic--claude-4.5-sonnet` or `gpt-5-mini`. **Not yet validated against a real tenant.** See [SAP AI Core](#sap-ai-core) |

**Opting a server into the Responses API.** Any OpenAI-shaped profile can set
`"dialect": "openai-responses"` to use `POST {base_url}/responses` instead of
Chat Completions. vLLM serves it:

```json
{"name": "house-vllm-responses", "extends": "vllm", "dialect": "openai-responses",
 "base_url": "http://VLLM-HOST:8000/v1"}
```

Built-in capabilities are conservative. Structured output, for example, is
declared absent on every self-hosted template, because vLLM and SGLang
enforce it only with guided decoding enabled. A server that accepts the
field and ignores it would turn a guaranteed-parseable answer into a
paragraph at run time. If your server enforces it, declare it in your
profile.

### A regional Vertex AI profile

Some partner models are served from one region only. Llama 4 Maverick, for
example, returns 404 at `global` and answers at `us-east5`. (It is also
[unsupported for tool use](/reference/tested-models/#llama-4-maverick-unsupported-for-tool-use).)
Give such a model a profile of its own rather than moving every model's
region:

```json
{"name": "vertex-maas-us-east5", "extends": "vertex-maas", "params": {"region": "us-east5"}}
```

### SAP AI Core

`sap-ai-core` reaches SAP's Generative AI Hub through its orchestration
service, the shape LiteLLM's `sap` provider and SAP's SDKs use. It reads
the credentials SAP issues in either form:

- **A service key**: the JSON from SAP BTP, in `AICORE_SERVICE_KEY`, as SAP
  issues it or nested under `"credentials"` as a Cloud Foundry binding holds
  it. It supplies the token endpoint, the client and the AI API URL.
- **Four values**: `AICORE_AUTH_URL`, `AICORE_CLIENT_ID`,
  `AICORE_CLIENT_SECRET` and `AICORE_BASE_URL` (the service key's `url` and
  `serviceurls.AI_API_URL`; SAP's `/oauth/token` and `/v2` paths are added
  when they're missing).

Setting both is refused, naming the variables, rather than one silently
winning. `AICORE_RESOURCE_GROUP` picks the resource group (default
`default`), and `AICORE_DEPLOYMENT_ID` pins a deployment instead of the
newest running orchestration one. Only client-secret service keys work; an
X.509 key is refused with a message saying so.

```bash
export AICORE_SERVICE_KEY="$(cat sap-service-key.json)"
export AICORE_RESOURCE_GROUP=team-a
```

Orchestration's own modules (content filtering, data masking, grounding,
translation) are yours to configure. Put them under `extra_body.modules`,
which is merged into the request's `config.modules`; every other
`extra_body` key goes to the model's parameters:

```json
{"name": "sap-masked", "extends": "sap-ai-core",
 "extra_body": {"reasoning_effort": "low",
                "modules": {"masking": {"providers": [{"type": "sap_data_privacy_integration",
                  "method": "anonymization", "entities": [{"type": "profile-email"}]}]}}}}
```

Two limits to know about:

- The newest message in a request is rendered by orchestration's template
  for `{{?name}}` placeholders, so a newest message containing `{{?` is
  refused rather than sent to be rewritten. Earlier messages pass through
  untouched.
- SAP bills in capacity units, and core-models ships no `sap-ai-core`
  prices. A model whose id matches a first-party row without a backend
  prefix (`gemini-2.5-pro`) is estimated at that list price by the
  catalog's fallback; others (`gpt-5-mini`, priced only as `openai/…`, and
  SAP's own spellings such as `anthropic--claude-…`) are unpriced. Declare
  `models[].rates` for your contract rate.

The request and response shapes follow SAP's Orchestration v2 API
specification and SAP's Cloud SDK for AI, and are tested against a fake
tenant. They have not yet been checked against a real SAP AI Core tenant.

## Opening a profile

```go
p, err := coremodels.Open(ctx, prof, coremodels.Options{})
m, err := p.Model(ctx, "Qwen/Qwen3-Coder-Next")   // an llm.LLM
```

`Open` resolves the profile and picks its dialect. `Model` refuses an id
the profile doesn't serve. A dialect that isn't built yet fails at `Open`
and says which phase brings it.

## When a profile is checked

Both checks happen before any request is sent:

- **`Validate`** checks the shape:
  - known dialect and a well-formed URL;
  - every placeholder has a param, and the auth block is consistent;
  - models are listed unless `open_models` is set;
  - every tier names a model the profile serves.

  It reports every problem at once, and each message names the profile.
- **`Resolve`** checks the profile against the environment: every
  `${VAR}` is set, and the API key or Google credentials are found. A
  missing value is named by its variable, never shown by its content.

The strict JSON reader `DecodeJSON` rejects unknown keys, so a misspelled
`base_ulr` is an error instead of a silently missing value.
