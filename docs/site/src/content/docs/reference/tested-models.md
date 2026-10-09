---
title: Tested models
description: Which models core-models has been run against, what each kind of test proves, and the results — including the tool-calling parity run against Claude and the one model we leave out.
---

Every model on this page has been run through core-models against its real
server. There are two kinds of test. They answer different questions, so
read them separately.

## The two kinds of test

### Conformance: does the adapter speak this server correctly?

Three scenarios per server: a plain answer, a full tool-call round trip
(the model calls a tool, gets the result, answers from it), and a streamed
answer. Each scenario is run once against the real server, and every HTTP
exchange is recorded. The recording is then **replayed offline on every
pull request**. The adapter must send exactly the request it sent when
recorded, and produce the same answers from the same responses.

A pass means core-models handles that server's dialect: its tool-call
format, reasoning fields, usage fields, streaming and errors. It says
nothing about how good the model is.

### Tool-calling parity: is the model good enough for an agent?

[mast](https://github.com/go-steer/mast)'s judged eval corpus: 31
Kubernetes incidents (CrashLoopBackOff, OOMKilled, DNS failures,
NetworkPolicy, mTLS, and so on), ported from the LangChain SRE agent. The
model works each incident with mast's real tool catalog. The tools answer
from per-incident fixtures, not a live cluster. A Claude model grades the
final answer. The same corpus runs nightly against Claude, which is the
baseline.

| Metric | What it measures |
|---|---|
| **Intent** | Share of the investigative questions the corpus expects for an incident (is the pod crashing, what do its logs say, what changed) that the model's tool calls actually answered. Any tool that answers a question counts, so it rewards investigating, not naming a particular tool. **The headline number** |
| **Quality** | The grader's 1–5 score of the final answer against the expected diagnosis, normalized to 0–1. LLM-judged, so noisier than intent |
| **Tools / incident** | Distinct tools the model called per incident. It shows *how* a model investigates: broadly, or one look and an answer |
| **Malformed calls** | Calls naming an unknown tool, missing or inventing an argument, passing a value outside a declared enum, or erroring. Counted against all calls made. This is the adapter-and-model correctness floor |
| **Consequential misses** | Questions the corpus expected answered, that a tool in the catalog would have answered, that the run never asked. Skipping a redundant tool is not a miss |

Severity accuracy is also on the board, but it is a diagnostic: the corpus
doesn't define most of its severity labels, so it is not a comparison
number.

## Results

### Parity run: 2026-10-09

All runs went through the built-in `vertex-maas` profile, core-models
v0.2.0, all 31 incidents. Every run used the `claude-haiku-4-5` grader on
Vertex AI, and is compared with the previous night's `claude-opus-5` board.

| Model | Intent | Quality | Tools / incident | Malformed calls | Consequential misses | Status |
|---|---|---|---|---|---|---|
| **claude-opus-5** (baseline) | 0.973 | 0.855 | 8.2 | 0 / 253 | 1 | — |
| `zai-org/glm-5.2-maas` | 0.957 | 0.935 | 8.9 | 0 / 277 | 2 | **At parity** |
| `moonshotai/kimi-k2-thinking-maas` | 0.977 | 0.806 | 5.4 | 0 / 168 | 1 | **At parity** |
| `qwen/qwen3-coder-480b-a35b-instruct-maas` | 0.896 | 0.694 | 3.4 | 0 / 106 | 7 | Works, below parity |
| `openai/gpt-oss-20b-maas` | 0.670 | 0.468 | 1.6 | 0 / 49 | 22 | Works, small tier |
| `meta/llama-4-maverick-17b-128e-instruct-maas` | 0.224 | 0.460 | 0.3 | 0 / 8 | 51 | **Unsupported for tool use** (below) |

What it shows:

- **The provider path is correct.** Zero malformed calls in 858, across five
  vendors.
- **GLM 5.2 and Kimi K2 Thinking match Claude on intent.** GLM also scores
  higher on judged quality.
- **The gap below parity is how broadly a model investigates.** GLM calls as
  many tools as Claude under the same prompts. Qwen and gpt-oss usually
  take one look and answer, and lose intent on the complex incidents.
- **Tiers route on a new vendor.** A profile mapping frontier to GLM 5.2,
  mid to Qwen3 Coder and small to gpt-oss-20b ran each tier on its own
  model. Checking that each tier is *billed* at its own rate waits until
  these models have prices.

Read these numbers carefully:

- **One run per model**, so there is no variance estimate yet. Repeated
  runs come next.
- **The baseline is a different day's run**, not interleaved with these.
- **"At parity" means intent within a few hundredths of Claude on this
  corpus.** That is evidence for unattended Kubernetes triage. It is not a
  general ranking of the models.

### Llama 4 Maverick: unsupported for tool use

Llama 4 doesn't emit structured tool calls itself. It writes a Python-style
call list, such as `[k8s_triage_workload(scope="web/frontend-v2")]`, and
the server converts that text into tool calls. That only works when the
whole reply is the call list.

Under mast's tool catalog it mostly wasn't. On 23 of 31 incidents no tool
ran. In 22 of those the call was written inside a sentence ("First, I'll
check the snapshot using `[k8s_triage_workload(...)]`…"), and in one prose
followed the call. Vertex AI's parser rightly doesn't treat that as a call.
The model always knew which tool it wanted; it didn't follow its own format.

core-models won't recover such calls from text. A strict parser would
rescue one incident in 23. A lenient one would execute calls the model
only described as a plan, which is the wrong trade for an agent that can
change a cluster. Llama 4 Maverick still passes conformance: short,
single-tool requests work. It is left out for tool-using workloads.
[core-models #12](https://github.com/go-steer/core-models/issues/12) has
the breakdown. It is also served only from `us-east5`.

### Conformance corpus

| Server | Model | Notable behaviour the adapter handles |
|---|---|---|
| Ollama 0.9.6, local | `qwen3:1.7b` | Reasoning inline as `<think>` tags, split out by `reasoning_format: think_tags` |
| Vertex AI, global | `openai/gpt-oss-20b-maas` | Reasoning in `reasoning_content`; prompt-cache hits reported; rejects forced tool choice, so the built-in profile downgrades it to `auto` |
| Vertex AI, global | `moonshotai/kimi-k2-thinking-maas` | Tool-call ids of the form `functions.<name>:<n>` |
| Vertex AI, global | `qwen/qwen3-coder-480b-a35b-instruct-maas` | No reasoning output; cached tokens reported |
| Vertex AI, global | `zai-org/glm-5.2-maas` | Text and a tool call in the same turn |
| Vertex AI, `us-east5` | `meta/llama-4-maverick-17b-128e-instruct-maas` | 404 at `global`; needs a regional profile |
| vLLM 0.31.0 on GKE (RTX PRO 6000), over PSC | `openai/gpt-oss-120b` | Reports reasoning tokens separately. Cached tokens need `--enable-prompt-tokens-details`. Self-hosted with [`deploy/gke-vllm`](/reference/self-hosting-on-gke/) |

All seven replay on every pull request.

## Reproducing

- **Conformance:** `go test -tags live -run TestLive -v .` in core-models,
  with `CORE_MODELS_LIVE_PROFILE` and `CORE_MODELS_LIVE_MODEL` set. Add
  `CORE_MODELS_LIVE_RECORD=<dir>` to record a new corpus.
- **Parity:** mast's judged tier. For example:

  ```sh
  go run ./internal/evals/cmd/evals --tier judge \
    --provider vertex-maas --model zai-org/glm-5.2-maas
  ```

  Add `--rows` for a cheap subset first. The board reports the tokens each
  model used.
