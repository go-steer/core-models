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
| `base_url` | The API root. May contain `{param}` placeholders and `${VAR}` references. Required for the OpenAI dialects |
| `params` | Values for `base_url`'s placeholders. Each may be `${VAR}` or `${VAR:-default}` |
| `auth.kind` | `api_key` or `bearer` (with `auth.env` naming the variable that holds it), `google_adc`, or `none` |
| `auth.scopes` | OAuth scopes for `google_adc`. Defaults to cloud-platform |
| `backend` | The name prices are keyed on. Defaults to `name`, except that a profile extending `vertex-maas` keeps `vertex-maas` |
| `open_models` | Accept any model id, not only the listed ones. Set on every self-hosted template |
| `capabilities` | `response_schema`, `reasoning_echo`, `parallel_tool_calls`, `server_tools`, `streaming`. Each is true, false, or unset; unset reads as absent |
| `usage.cached_tokens` | `unreliable` records cached tokens as not reported even when the server sends them |
| `reasoning_format` | `think_tags` for a server that puts reasoning inline as a leading `<think>…</think>` block (Ollama; vLLM or SGLang without a reasoning parser). The block becomes a reasoning part instead of answer text |
| `metrics_url` | The server's Prometheus endpoint, for the optional KV-cache sampler |
| `models` | `id`, plus an optional `tier` (`small`, `mid` or `frontier`) and `context_window` |
| `tiers` | The model for each tier the profile can fill |

`extends` merges `params` and `tiers` key by key. It replaces `models` and
`auth` as a whole, and overrides each capability on its own.

## Built-in profiles

| Profile | Dialect | Endpoint | Auth | Notes |
|---|---|---|---|---|
| `vertex-maas` | openai-chat | Vertex AI's OpenAI-compatible endpoint for the project and region | Google ADC | Project from `GOOGLE_CLOUD_PROJECT`. Region from `GOOGLE_CLOUD_LOCATION`, default `global`. Ids are publisher-qualified, such as `openai/gpt-oss-20b-maas` |
| `ollama` | openai-chat | `http://localhost:11434/v1` | none | Works with nothing set. `reasoning_format: think_tags` |
| `vllm` | openai-chat | *template: set `base_url`* | none | Parallel tool calls declared |
| `sglang` | openai-chat | *template: set `base_url`* | none | Parallel tool calls declared |
| `openai-compatible` | openai-chat | *template: set `base_url`* | none | For any other OpenAI-compatible endpoint; set `auth` too |

Built-in capabilities are conservative. Structured output, for example, is
declared absent on every self-hosted template, because vLLM and SGLang
enforce it only with guided decoding enabled. A server that accepts the
field and ignores it would turn a guaranteed-parseable answer into a
paragraph at run time. If your server enforces it, declare it in your
profile.

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
