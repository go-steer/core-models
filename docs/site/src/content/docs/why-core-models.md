---
title: Why core-models
description: Why provider support is a shared library rather than code inside mast or core-agent, and why it is not an existing multi-provider library.
---

## The problem it solves

[mast](https://github.com/go-steer/mast) and
[core-agent](https://github.com/go-steer/core-agent) are sibling Go agents
built on Google's Agent Development Kit (ADK). Both talk to Gemini and
Claude today, and each carries its own copy of the code that does it. The
copies started as one and have drifted apart, each fixing problems the
other still has.

Both now need the same next set of models:
- OpenAI;
- the partner models Vertex AI serves, such as Grok, DeepSeek, Qwen and
  gpt-oss;
- open-weight models on self-hosted servers such as vLLM, SGLang and
  Ollama.

Writing those adapters twice, or writing them once and copying them
across, would make the drift worse.

core-models holds that code once, and both products import it.

## What it is not

- **Not an agent framework.** It has no agent loop, no tools runtime and
  no sessions. Those belong to ADK and to the products built on it.
  core-models stops at "send this request to that model and tell me what
  it cost".
- **Not a proxy.** Nothing runs between the agent and the model. If you
  already run LiteLLM or OpenRouter, core-models reaches it like any other
  OpenAI-compatible endpoint, but it never requires one.
- **Not product policy.** Which model a "small" task gets, what a budget
  ceiling is, how a CLI flag is spelled: each product decides those.
  core-models provides the facts they decide with.

## Why not an existing library

The closest existing Go library is
[fantasy](https://github.com/charmbracelet/fantasy), which already covers
many of the same providers. mast measured it as a dependency and declined
it, for reasons that carry over here:

- **Its contract is its own model interface and agent loop**, not ADK's.
  Putting it under an ADK agent means translating every request and
  response twice.
- **Its usage counts are plain integers**, so a count the provider never
  reported looks the same as a reported zero. Keeping those apart is the
  whole point of the [usage record](/concepts/usage-record/).
- **Importing it would move the Gemini and Anthropic SDK versions** that
  the existing adapters are tested against.

core-models borrows fantasy's best ideas with attribution: one test suite
run against every provider, header-aware retry, and its catalog of ways
"OpenAI-compatible" servers differ from OpenAI.
