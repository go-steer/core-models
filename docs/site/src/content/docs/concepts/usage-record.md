---
title: The usage record
description: The usage.Detail record every adapter attaches beside genai's usage metadata, and why every count in it is a pointer.
sidebar:
  order: 2
---

genai's usage struct carries prompt, cached, output, reasoning and total
token counts. That covers Gemini, nearly covers Claude, and falls short
for everything else:

- **It has no cache-write bucket.** Claude and OpenAI bill writing to a
  prompt cache at a premium, so a turn that warms a cache is underpriced.
- **It can't say "not reported".** A zero might mean the provider reported
  zero, or that it said nothing. Self-hosted servers often say nothing
  about cached tokens.

So every adapter also attaches a `usage.Detail` to the response's
`CustomMetadata`, under `usage.MetadataKey` (`core_models.usage`).

## Counts are pointers

| Field | Meaning |
|---|---|
| `CacheReadTokens` | Prompt tokens served from a provider-side cache |
| `CacheWriteTokens` | Prompt tokens written to a cache, every TTL |
| `CacheWrite1hTokens` | The one-hour share of the above, where it is billed differently |
| `ReasoningTokens` | Reasoning tokens the provider broke out |
| `ToolUseTokens` | Tool-use prompt tokens the provider broke out |

A nil count means the provider did not report it. A pointer to zero means
it reported zero. In JSON, an unreported count is omitted and a reported
zero is written as `0`.

## Identity fields

| Field | Meaning |
|---|---|
| `ServedModel` | What the backend said it ran. May not be something a price table understands, such as a resource path or a deployment name. The response's `ModelVersion` is the priceable id |
| `ProviderRequestID` | The id a support ticket needs |
| `Backend` | The provider profile that served the call: the key a price is looked up under |
| `Region` | The serving region, where the backend has one |

## Reading it back

`usage.FromMetadata` reads the record in both forms it can take:
- live, as the value an adapter attached;
- replayed, after a JSON round trip through an event log, as a generic map.

A product's meter can therefore use the same call on live and resumed
sessions.

The shape starts from the record mast shipped first (mast #352). mast and
core-agent each read it through their own meters, which keep their own
rules for clamping inconsistent counts.
