# Contributing to core-models

Thanks for your interest in contributing. By participating you agree to abide
by the [Code of Conduct](./CODE_OF_CONDUCT.md).

core-models is the provider layer [mast](https://github.com/go-steer/mast) and
[core-agent](https://github.com/go-steer/core-agent) share. Read
[`docs/design.md`](./docs/design.md) before proposing anything that changes
its shape, and [`AGENTS.md`](./AGENTS.md) for the house rules. Those rules
apply to humans too.

## Reporting bugs and requesting providers

- **Bugs:** [open an issue](https://github.com/go-steer/core-models/issues/new).
  Include:
  - the module and version;
  - which product you saw it through (mast, core-agent, or a direct import);
  - the provider and model;
  - the smallest reproduction you can manage.

  If a bug only appears through one product, say which.
- **A provider or model you need:** check the
  [roadmap](https://go-steer.github.io/core-models/roadmap/) first. If it isn't
  there, open an issue with the deployment you need it for. The order of work
  follows real demand.

## Pull requests

1. For anything beyond a small fix, open an issue first and agree the approach.
2. Branch off `main` (`feat/...`, `fix/...`, `docs/...`, `chore/...`) and keep
   the diff to one workstream.
3. Run every check CI runs:

   ```bash
   dev/ci/presubmits/all.sh
   dev/tools/docs-site.sh check   # if you touched docs/site (needs Node 22)
   ```

4. Open the PR against `main`. CI runs the same scripts, plus the
   `agent attribution` check.

### Commit messages

[Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`,
`docs:`, `test:`, `refactor:`, `chore:`, `ci:`. Add an optional scope, for
example `feat(openaichat): ...`. Keep the subject under about 70 characters and
explain *why* in the body.

### Developer Certificate of Origin

Sign off every commit under the [DCO](https://developercertificate.org/) with
`git commit -s`. The name and email must match your git config.

### No AI-agent attribution

Commits and PRs carry no AI-agent attribution. That means no `Co-authored-by:`
naming an AI tool, no "Generated with" footer, and no agent as commit author.
The `agent attribution` check scans every commit on your branch plus the PR
title and body, because a squash merge copies each branch commit's co-authors
into `main`. Many AI coding tools add a trailer by default; turn it off. A
human co-author is fine.

### License headers

Every source file carries the Apache 2.0 header attributed to Google LLC (see
any `.go` file). `golangci-lint` enforces it on Go files.

### Tests

- Tests live next to the code and run offline, with no credentials.
- An adapter is tested against recorded provider responses, not live calls. A
  live test skips cleanly without credentials.
- A change to `usage.Detail` handling asserts both the counts that were
  reported and the ones that stay unreported (nil).
- A new feature without a test is not done, and a bug fix without a regression
  test lets the bug come back.

## License

By contributing, you agree that your contributions will be licensed under the
[Apache License 2.0](./LICENSE).
