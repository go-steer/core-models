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
| **Tool calls / incident** | Every tool call the model made per incident, repeats included |
| **Distinct tools / incident** | Different tools the model called per incident. It shows *how* a model investigates: broadly, or one look and an answer. A large gap from the previous column means repetition |
| **Malformed calls** | Calls naming an unknown tool, missing or inventing an argument, passing a value outside a declared enum, or erroring. Counted against all calls made. This is the adapter-and-model correctness floor |
| **Consequential misses** | Questions the corpus expected answered, that a tool in the catalog would have answered, that the run never asked. Skipping a redundant tool is not a miss |

Severity accuracy is also on the board, but it is a diagnostic: the corpus
doesn't define most of its severity labels, so it is not a comparison
number.

## Results

### Managed models on Vertex AI: 2026-10-09

All runs went through the built-in `vertex-maas` profile on all 31
incidents, graded by `claude-haiku-4-5` on Vertex AI. Each managed model
ran three times. The baseline is three consecutive `claude-opus-5`
nightlies. Values are means, with the range across runs in brackets.

| Model | Runs | Intent | Quality | Tool calls / incident | Distinct tools / incident | Malformed calls |
|---|---|---|---|---|---|---|
| **claude-opus-5** (nightly baseline) | 3 | 0.973 (0.973–0.973) | 0.855 (0.839–0.871) | 8.0 | 7.0 | 0 / 744 |
| `zai-org/glm-5.2-maas` | 3 | 0.975 (0.957–0.984) | 0.941 (0.935–0.952) | 8.4 | 5.8 | 0 / 780 |
| `moonshotai/kimi-k2-thinking-maas` | 3 | 0.941 (0.912–0.977) | 0.815 (0.806–0.823) | 5.2 | 4.3 | 0 / 487 |
| `qwen/qwen3-coder-480b-a35b-instruct-maas` | 3 | 0.890 (0.880–0.896) | 0.672 (0.645–0.694) | 3.2 | 2.7 | 0 / 295 |
| `openai/gpt-oss-20b-maas` | 3 | 0.663 (0.648–0.670) | 0.503 (0.468–0.524) | 1.5 | 1.4 | 0 / 136 |
| `meta/llama-4-maverick-17b-128e-instruct-maas` | 1 | 0.224 | 0.460 | 0.3 | 0.3 | 0 / 8 |

| Model | Status |
|---|---|
| `zai-org/glm-5.2-maas` | **At parity**: intent matches Claude on every run, and quality is higher |
| `moonshotai/kimi-k2-thinking-maas` | **Close, below parity.** One run matched Claude; the three-run mean is 0.03 behind |
| `qwen/qwen3-coder-480b-a35b-instruct-maas` | Works, below parity |
| `openai/gpt-oss-20b-maas` | Works, small tier |
| `meta/llama-4-maverick-17b-128e-instruct-maas` | **Unsupported for tool use** (see below) |

What it shows:

- **The provider path is correct.** No malformed calls in 1,706 across the
  five open models' runs.
- **GLM 5.2 is the one model at parity with Claude.** Its intent ranged
  from 0.957 to 0.984 against Claude's steady 0.973, with higher judged
  quality on every run.
- **Below parity, the difference is how broadly a model investigates.**
  Claude reads about 7 distinct tools per incident and GLM about 6. Qwen and
  gpt-oss usually take one or two looks and answer, and lose intent on the
  complex incidents.
- **Tiers route on a new vendor.** A profile mapping frontier to GLM 5.2,
  mid to Qwen3 Coder and small to gpt-oss-20b ran each tier on its own
  model. Checking that each tier is *billed* at its own rate waits until
  these models have prices.

**Tool calls per incident** counts every call, repeats included.
**Distinct tools per incident** counts different tools. A large gap between
the two means the model repeated itself.

### Self-hosted on vLLM, and one model both ways: 2026-10-09

These runs used vLLM 0.31.0 on one RTX PRO 6000 in GKE, reached over Private
Service Connect ([Self-hosting on GKE](/reference/self-hosting-on-gke/)), with
one run per configuration. Gemma 4 also ran on Vertex AI: the same weights,
behind a different server.

| Model and server | Incidents completed | Intent | Quality | Tool calls / incident | Distinct tools / incident | Malformed calls |
|---|---|---|---|---|---|---|
| `openai/gpt-oss-120b`, our vLLM | 31 / 31 | 0.874 | 0.911 | 7.7 | 3.4 | 1 / 241 |
| `google/gemma-4-26B-A4B-it`, our vLLM, thinking off | 24 / 31 | 0.858 | 0.740 | 3.0 | 2.6 | 0 / 72 |
| `google/gemma-4-26B-A4B-it`, our vLLM, thinking on, vLLM example template | 31 / 31 | 0.933 | 0.685 | 26.5 | 3.6 | 0 / 820 |
| `google/gemma-4-26B-A4B-it`, our vLLM, thinking on, model's own template | 31 / 31 | 0.885 | 0.702 | 11.3 | 3.6 | 0 / 351 |
| `google/gemma-4-26b-a4b-it-maas`, Vertex AI, thinking on | 31 / 31 | 0.917 | 0.677 | 3.6 | 2.6 | 0 / 112 |

What it shows:

- **gpt-oss-120b works well self-hosted.** Quality is above the Claude
  baseline. Its intent is lower, and part of its breadth is repetition
  (7.7 calls over 3.4 distinct tools). Its one malformed call named
  `k8s_resource_spec.json`: gpt-oss's output format leaked a
  constrain-to-JSON marker into the tool name through vLLM's parser. mast
  rejected it as an unknown tool, which is correct. core-models does not
  rewrite tool names.
- **Gemma 4 needs thinking on for agentic work.** With thinking off (the
  default), it repeated one identical call (441 times on one incident) until
  the 65k context overflowed, on 7 of 31 incidents. Thinking is a
  per-request switch, set in the profile with `extra_body`:
  `{"chat_template_kwargs": {"enable_thinking": true}}`.
- **The same weights behaved differently on two servers, and the chat
  template explains much of it.** With thinking on, Gemma 4 scored
  about the same on vLLM and Vertex AI, but on vLLM it repeated tools far
  more. vLLM's bundled example template predates Google's fix for
  tool-calling loops. Switching to the model's own template (now the
  fixture's default) cut model calls from 851 to 382 across the run.
  Vertex AI still made only 135. The rest of the gap is open; sampling
  defaults are the next suspect.

Read these numbers carefully:

- **Self-hosted results are one run each.** The managed models' ranges show
  how much a single run can move.
- **The baseline is the nightly board**, not interleaved with these runs.
- **"At parity" means intent within Claude's range on this corpus.** That
  is evidence for unattended Kubernetes triage, not a general ranking of the
  models.

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
| vLLM 0.31.0 on GKE, over PSC | `google/gemma-4-26B-A4B-it` | Gemma's own tool-call format, parsed by vLLM's `gemma4` parser. Recorded with thinking off |
| Vertex AI, `global` only | `google/gemma-4-26b-a4b-it-maas` | Thinking on via `extra_body`. `us-central1` refuses it as global-only |

All nine replay on every pull request.

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
