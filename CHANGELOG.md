# Changelog

## Unreleased

### Added

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
