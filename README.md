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

**Status: pre-release.** The provider contract, the usage record and the two
ADK adapter modules exist. The first new provider, OpenAI-compatible Chat
Completions, is next. See the
[roadmap](https://go-steer.github.io/core-models/roadmap/).

- **Docs:** <https://go-steer.github.io/core-models/>
- **Design:** [`docs/design.md`](./docs/design.md)
- **Contributing:** [`CONTRIBUTING.md`](./CONTRIBUTING.md). Agents, read
  [`AGENTS.md`](./AGENTS.md).

## Modules

| Module | For |
|---|---|
| `github.com/go-steer/core-models` | The contract (`llm`), the usage record (`usage`), and later the providers. Never imports ADK |
| `github.com/go-steer/core-models/adkv1` | Projects on `google.golang.org/adk` v1 |
| `github.com/go-steer/core-models/adkv2` | Projects on `google.golang.org/adk/v2` |

## License

[Apache 2.0](./LICENSE).
