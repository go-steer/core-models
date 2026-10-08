# AGENTS.md

Instructions for AI coding agents (Claude Code, Gemini CLI, Codex, Cursor and
others) working in this repo. Read all of it before changing anything
substantive. Claude Code reads this file natively, so there is deliberately no
`CLAUDE.md`. If you add one, its first line must be `@AGENTS.md`, so the two
can never diverge.

---

## What this repo is

**core-models** is the shared LLM provider layer for
[`go-steer/mast`](https://github.com/go-steer/mast) and
[`go-steer/core-agent`](https://github.com/go-steer/core-agent). It contains:

- provider adapters, one per wire format (genai, Anthropic Messages, OpenAI
  Chat Completions, OpenAI Responses);
- provider profiles;
- the normalized usage record every adapter fills in;
- later, the price catalog.

Both products import it. Neither carries its own copy.

It exists because the two products' Gemini and Anthropic adapters had drifted
apart in both directions, and both needed the same next set of providers:
OpenAI, the Vertex AI partner models, and self-hosted vLLM, SGLang and Ollama.
[`docs/design.md`](./docs/design.md) is the design and records the settled
decisions (§2). Read it before proposing a change of shape.

**Status:** pre-release, phase L0. What exists:

- the `llm` contract;
- the `usage` record;
- the `adkv1` and `adkv2` adapter modules;
- CI;
- the docs site.

No provider adapters yet. [`docs/design.md`](./docs/design.md) §11 lists the
phases.

## Reading order

1. [`README.md`](./README.md): the public face.
2. [`docs/design.md`](./docs/design.md): decisions (§2), module layout (§3),
   contract (§4), profiles (§5), usage (§6), dialects (§7), phases (§11).
3. mast's [`docs/model-support-design.md`](https://github.com/go-steer/mast/blob/main/docs/model-support-design.md):
   the **requirements** every provider must meet (R1–R8), the seams inventory,
   and the fantasy prior-art survey (§10). That document stays authoritative
   for requirements; this repo owns the implementation.
4. The docs site source under [`docs/site/`](./docs/site/), deployed to
   [go-steer.github.io/core-models](https://go-steer.github.io/core-models/).

## Sibling repos

| Repo | Relationship |
|---|---|
| [`go-steer/mast`](https://github.com/go-steer/mast) | Consumer, on ADK v2, through `adkv2`. Owns the provider requirements doc |
| [`go-steer/core-agent`](https://github.com/go-steer/core-agent) | Consumer, on ADK v1, through `adkv1` |

**Sync discipline.** Once a provider package lives here, fixes to it land
**here**: a core-models release, then a version bump in each product. Never
patch a product's vendored or legacy copy and port it later; that is the drift
this repo exists to end. Until a package is extracted (Anthropic at L4, Gemini
at L5), fixes to it keep landing in the products. mast's
[`docs/sibling-sync.md`](https://github.com/go-steer/mast/blob/main/docs/sibling-sync.md)
records the handover.

---

## House rules

These are the same rules mast and core-agent follow, plus four that exist
because this is a library with two consumers.

### 1. No AI-assistant attribution on commits, PRs, or artifacts

Work is committed by its human author, whatever tooling helped produce it. Do
**not** add:

- `Co-Authored-By:` (or any other credit trailer) naming an AI assistant,
  agent or model;
- "Generated with <tool>" badges, footers, emoji trailers or tool-marketing
  links;
- any other marketing-style trailer in commit messages, PR titles or bodies,
  release notes, or docs.

Use the human's `user.name` and `user.email`. The change is theirs; you're the
typing. This is enforced by the `agent attribution` workflow, which scans every
commit on the PR branch plus the PR title and body, and by
`dev/ci/presubmits/attribution.sh` locally. Many tools add a co-author trailer
by default; for Claude Code, set `"attribution": {"commit": "", "pr": ""}` in
settings.

### 2. Apache 2.0 license header on every source file

Every `.go`, `.sh`, `.yml`, `.mjs` and `.py` file gets the 13-line Google LLC
header, in the comment syntax of the file type. `goheader` enforces it on Go
files. Code moved from mast or core-agent carries an
`Originally derived from go-steer/<repo>@<sha>[:<path>]` comment as its own
comment group **below** the header.

Code derived from `charmbracelet/fantasy` (mast model-support §10.2) keeps
Charm's copyright line on the file, and the repo must carry fantasy's `NOTICE`
text the first time such code lands.

### 3. Public names only, no internal codenames

Every name in a committed artifact must be public. For the adjacent
interactive-IDE work the public name is **Antigravity**. If you only know
something by an internal name, stop and ask.

### 4. Docs site alongside user-visible changes

A change a consumer can see must update both the design surface
([`docs/design.md`](./docs/design.md), README) and the site
(`docs/site/src/content/docs/`). Examples: a new package, a changed contract,
a new provider or profile, a changed dependency floor. Build with
`dev/tools/docs-site.sh check`, the same command CI runs.

### 5. Scratch files under `/tmp`, never `$HOME`

Session state, recorded fixtures in progress, logs: use `os.TempDir()` or
`/tmp`.

### 6. Run presubmits before pushing

`dev/ci/presubmits/all.sh` runs every check CI runs, sequentially. A local pass
means a CI pass. Don't ship preventable red builds.

### 7. Respect deferrals

If [`docs/design.md`](./docs/design.md) puts something in a later phase or out
of scope, don't build it early without a consumer asking for it.

### 8. The core module never imports ADK

This is decision D2. The core module (`llm/`, `usage/` and every future
provider package) depends on genai and vendor SDKs only. ADK enters only
through `adkv1/` and `adkv2/`. If the core reached an ADK major, every consumer
would get that major, and mast (v2) and core-agent (v1) could not both import
it. `dev/ci/presubmits/core-no-adk.sh` enforces this over the full dependency
graph, including tests.

### 9. The two adapter modules stay in lockstep

`adkv1` is `adkv2` with the import path changed, and nothing else. **Edit
`adkv2`**, then run `dev/ci/presubmits/shims-lockstep.sh --fix` to regenerate
`adkv1`. The presubmit fails on any other difference. Write comments in those
files so they read correctly after the rewrite: say "ADK v2" in prose, never
the package name of the other shim.

### 10. Dependency floors are the consumers' floors

A library's `require` lines become minimums for everyone who imports it. In the
core module, require the **lowest** version of genai, anthropic-sdk-go or
openai-go that either product uses. Each adapter module requires its product's
ADK version. That way importing core-models never upgrades an SDK underneath a
product's own tests.

Raise a floor only for a security fix, or for an API a new adapter needs.
Record why in the docs site's
[modules page](./docs/site/src/content/docs/reference/modules.md). Dependabot
proposes bumps; in the core module, treat each one as raising both products'
minimum, not as a routine patch. Before raising one, check both products'
current versions (`grep <module> go.mod` in each).

### 11. Not reported is not zero

Every count in `usage.Detail` is a pointer. An adapter sets a count only when
the provider reported it, and leaves it nil otherwise. Never write `0` to mean
"unknown". This distinction is requirement R3 and the reason the record exists.
A test that asserts a count must also assert the unreported ones stay nil.

### 12. No product policy

Tier promotion, budgets, compaction thresholds, CLI flags, config-file
locations and telemetry labels belong to the products. If a change needs to
know which product is calling, it belongs in that product. Expose the fact and
let the product decide.

---

## Layout

```
llm/                 the contract: LLM, Request, Response (mirrors ADK's model types)
usage/               usage.Detail, the normalized usage record
callctx/             per-call context markers shared with the products
retry/               HTTP-layer retry: Policy.Transport, Record
adkv1/  (module)     adapter to google.golang.org/adk v1  — core-agent
adkv2/  (module)     adapter to google.golang.org/adk/v2  — mast
docs/design.md       the design; decisions in §2
docs/site/           Astro + Starlight user docs
dev/tools/           pinned tools: lint-go, shell-lint, docs-site.sh,
                     verify-no-agent-attribution, common.sh (the module list)
dev/ci/presubmits/   one script per CI check; all.sh runs them all
scripts/             verify-internal-links.py (docs site)
.github/workflows/   thin delegators to dev/ci/presubmits/
```

Future packages (`profile/`, `auth/`, `dialect/*`,
`kvmetrics/`, `pricing/`, `toolwire/`, `conformance/`) are laid out in
[`docs/design.md`](./docs/design.md) §3. Create them in the phase that fills
them, not before.

## Build and test

```bash
dev/ci/presubmits/all.sh              # everything CI runs
dev/ci/presubmits/test.sh             # go test -race in every module
dev/ci/presubmits/lint.sh             # golangci-lint v2.12.1, the siblings' config
dev/ci/presubmits/shims-lockstep.sh --fix   # after editing adkv2
dev/tools/docs-site.sh check          # docs site build + link check (needs Node 22)
```

The repo has three Go modules. `dev/tools/common.sh` lists them in
`GO_MODULES`, and every Go presubmit iterates that list. A new nested module
is one line there. The adapter modules build against the core in this repo
through a `replace ../` directive. Consumers ignore that directive, so it is
safe to commit. Don't add a `go.work`; it is gitignored.

Tests run offline and need no credentials. Tests that call a live provider
skip cleanly when their credentials are absent, and sit behind a build tag
once they exist. Recorded fixtures replace live calls in presubmit
([`docs/design.md`](./docs/design.md) §10).

## How to commit and push

Never set `user.email`/`user.name` in repo config. Pass the human's identity per
command:

```bash
git -c user.email="<human's email>" -c user.name="<human's name>" commit -s -m "..."
```

- **Conventional Commits:** `feat(openaichat): ...`, `fix(adkv2): ...`,
  `docs(design): ...`, `chore(deps): ...`. The subject says what changed and
  stays under about 70 characters; the body says *why*.
- **DCO sign-off** (`-s`) on every commit, as in core-agent. A human
  `Signed-off-by:` is fine; an agent one is not (rule #1).
- **PRs** go against `main`, one workstream per PR. The body covers
  motivation, what changed, and how to verify. No attribution footer.

## How to release

Each module is tagged separately (Go's nested-module convention), and the core
always goes first:

1. Tag the core: `vX.Y.Z`.
2. In `adkv1/go.mod` and `adkv2/go.mod`, bump the `github.com/go-steer/core-models`
   requirement to `vX.Y.Z`. Keep the `replace` directive. Merge that change.
3. Tag the adapters: `adkv1/vX.Y.Z` and `adkv2/vX.Y.Z`.
4. Add a CHANGELOG entry per release.
5. Open the bump PRs in mast and core-agent.

Pre-1.0, a minor version may break the API. Say so in the CHANGELOG under
**API Change**, as mast does.

## Common foot-guns

- **Importing an ADK package from the core module "just for a type".** Mirror
  the type in `llm/` instead. The presubmit will catch it, but only after you've
  built on it.
- **Editing `adkv1` by hand.** It gets overwritten by `--fix`. Edit `adkv2`.
- **A count of `0` where the provider said nothing** (rule #11).
- **Upgrading genai in the core module because Dependabot asked** (rule #10).
- **Building a provider here that a design phase hasn't reached** (rule #7).
  Check §11 first.
- **Cross-repo links** use full GitHub URLs
  (`https://github.com/go-steer/<repo>/blob/main/...`). In-repo links are
  relative.
- **Modifying `LICENSE`.** Don't.
