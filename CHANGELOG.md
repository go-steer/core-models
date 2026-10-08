# Changelog

## Unreleased

### Added

- **`auth`:** resolves a profile's credential when the profile is opened:
  an API key or bearer token from a named variable, Google ADC, or none.
  `BearerTransport` fetches a fresh token per request. Secrets never appear
  in errors or `String()`.
- **`profile`:** the profile schema with `Validate`, which reports every
  problem at once, and `Resolve`, which fills `${VAR}` and `{param}`
  placeholders and finds credentials before any request.
  - `extends` starts a profile from a built-in.
  - `Find` looks up declared profiles before the built-ins.
  - `DecodeJSON` rejects unknown keys.
  - Built-ins: `vertex-maas`, `ollama`, and the `vllm`, `sglang` and
    `openai-compatible` templates.
- **`callctx`:** the per-call context markers adapters and products share:
  side call, no built-ins, no prompt cache, and the prior-success record.
  They come from core-agent's `pkg/models`, so its helpers can become
  aliases.
- **`retry`:** `Policy.Transport`, an `http.RoundTripper` that retries 408,
  429, 5xx gateway errors and dropped connections.
  - It honors `retry-after-ms`, `retry-after` (seconds or HTTP date) and
    `x-should-retry`.
  - It hands back, without waiting, a response whose server asks for longer
    than `MaxHeaderDelay`.
  - Each call's retries are recorded on a `Record` for the adapter to stamp
    onto the response.
- **`llm`:** the provider contract (`LLM`, `Request`, `Response`). It mirrors
  ADK's `model` package field for field and imports no ADK, so one core serves
  both ADK majors.
- **`usage`:** `Detail`, the normalized usage record adapters attach beside
  genai's usage metadata. Counts are pointers, so "not reported" stays distinct
  from zero. `FromMetadata` reads it live and after a JSON round trip.
- **`adkv1` and `adkv2` modules:** `Wrap` and `FromADK` adapt the contract to
  ADK v1 (core-agent) and ADK v2 (mast). Reflection tests fail the build if ADK
  adds a field the mirror lacks.
- **CI and tooling:**
  - presubmits for build, vet, gofmt, golangci-lint, `go mod tidy`,
    govulncheck, shell-lint and agent attribution;
  - two checks specific to this repo: the core module imports no ADK, and the
    two adapter modules stay in lockstep.
- **Docs site** under `docs/site`, deployed to
  <https://go-steer.github.io/core-models/>.
