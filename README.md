# core-models

One LLM provider layer for Go agents built on Google's
[Agent Development Kit](https://github.com/google/adk-go). It is shared by
[mast](https://github.com/go-steer/mast) and
[core-agent](https://github.com/go-steer/core-agent).

**What's in it:**
- provider adapters for Gemini, Claude, OpenAI, the Vertex AI partner models,
  and self-hosted model servers (vLLM, SGLang, Ollama);
- one usage record every adapter fills in, so tokens and cost look the same
  whichever model answered;
- thin adapter modules for ADK v1 and ADK v2.

**Status: pre-release (v0.x), in use.** mast and core-agent both run on it.
Four wire formats ("dialects") are built:

| Dialect | Package | Reaches |
|---|---|---|
| `openai-chat` | `dialect/openaichat` | Vertex AI partner models, xAI, vLLM, SGLang, Ollama and any OpenAI-compatible endpoint |
| `openai-responses` | `dialect/openairesp` | OpenAI, and any server that serves `/v1/responses` (xAI, vLLM) |
| `anthropic` | `dialect/anthropic` | Claude on Anthropic's API and on Vertex AI |
| `gemini` | `dialect/gemini` | Gemini on the Developer API and Vertex AI |

See the [roadmap](https://go-steer.github.io/core-models/roadmap/) for what
is still owed.

- **Docs:** <https://go-steer.github.io/core-models/>
- **Design:** [`docs/design.md`](./docs/design.md)
- **Contributing:** [`CONTRIBUTING.md`](./CONTRIBUTING.md). Agents, read
  [`AGENTS.md`](./AGENTS.md).

## Modules

| Module | For |
|---|---|
| `github.com/go-steer/core-models` | The contract (`llm`), the usage record (`usage`), profiles, the dialects above, the KV-cache sampler (`kvmetrics`) and the shared price catalog (`pricing`). Never imports ADK |
| `github.com/go-steer/core-models/adkv1` | Projects on `google.golang.org/adk` v1. **Deprecated** since v0.6.6, its last release; move to ADK v2 and `adkv2` |
| `github.com/go-steer/core-models/adkv2` | Projects on `google.golang.org/adk/v2` |

## License

[Apache 2.0](./LICENSE).
