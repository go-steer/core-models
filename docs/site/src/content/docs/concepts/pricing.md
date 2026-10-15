---
title: Pricing
description: The pricing package both products share — the layered catalog, (backend, model) rates, long-context tiers, declared rates for self-hosted models, and what is left to each product.
sidebar:
  order: 6
---

Package `pricing` turns a model call's tokens into dollars. It is the one
price catalog mast and core-agent share (phase L6): the `Rates` type and its
cost methods, a layered `Catalog`, a compiled-in table generated from
[LiteLLM's catalog](https://github.com/BerriAI/litellm), and the fetch that
refreshes it.

## Rates and cost

A `Rates` row is per million tokens: input, cache reads, cache writes at the
5-minute TTL, cache writes at the 1-hour TTL, and output. The cost methods
take the usage record's buckets:

```go
cost := rates.CostUSDWithCacheTTLs(uncached, cacheReads, cacheWrites, cacheWrites1h, output)
```

The input buckets don't overlap: pass `uncached = prompt − reads − writes`.
`cacheWrites1h` is the subset of `cacheWrites` written at the 1-hour TTL. An
unknown cache rate falls back to the input rate, and an unknown 1-hour rate to
the 5-minute one, so a missing field understates rather than bills tokens as
free.

### Long-context tiers

Some models price a long prompt higher: OpenAI above 272K tokens, Gemini
above 200K, some Claude models above 100K. A row with such a tier carries a
`LongContext`, the above-threshold rates as LiteLLM publishes them. When a
call's whole prompt (uncached, reads and writes together) is above the
threshold, every token of it, output included, bills at those rates;
`rates.At(promptTokens)` returns the rates for a call. A bucket the tier
doesn't publish scales with the input increase, so it doesn't stay at the
cheaper base price.

## The catalog

`NewCatalog(Options{...})` builds the layered view. A lookup takes the first
exact match, highest layer first. Only when no layer holds the exact id does
it fall back to a prefix (so `claude-opus-4-7-20991231` finds
`claude-opus-4-7`), again highest layer first, taking the longest prefix in
the first layer that has one:

| Layer | Source | Label (`LookupWithSource`) |
|---|---|---|
| 1 | `Options.CfgOverride`, the product's own config | `cfg-override` |
| 2 | `<AgentsDir>/pricing.json` | `project-file` |
| 3 | `<UserHome>/pricing.json`, section `manual` | `user-manual` |
| 4 | `<UserHome>/pricing.json`, section `external`, written by `Refresh` | `user-external` |
| 5 | the compiled-in table | `builtin` |
| 6 | `Options.Declared`, rates provider profiles declare | `profile-declared` |

A miss is a miss: `Lookup` returns `false`, and a product renders "$—". Nothing
here invents a rate for a model no layer prices.

### Rates belong to a (backend, model) pair

The same model can cost different amounts on different backends, and LiteLLM
prices them separately. `LookupFor(backend, model)` reads both the
`<backend>/<model>` row and the bare model id, layer by layer: in each layer
the qualified row first, then the bare one. Layer precedence comes before
qualification, so a bare override you write beats a qualified compiled-in row,
and the prefix fallback runs only after every layer misses on both exact keys.
The backend
is core-models' own name, as `profile.Profile.BackendName` and
`usage.Detail.Backend` report it: `anthropic`, `anthropic-vertex`, `gemini`,
`vertex`, `vertex-maas`, `openai`, `xai`, or a profile's own.

The compiled-in table carries both: bare rows for the Gemini and Claude
models, and qualified rows for every backend that serves them, plus the
Vertex AI partner models, OpenAI and xAI, which only have qualified rows.

### Self-hosted models and declared rates

A self-hosted model has no published price, so it's unpriced unless the
operator declares one in its profile:

```yaml
models:
  - id: google/gemma-4-26B-A4B-it
    rates: {input_per_mtok: 0.10, output_per_mtok: 0.40}
```

`DeclaredFromProfiles(profiles)` turns those into `Options.Declared`, keyed
`<backend>/<model>`, so a declaration prices the model only on the profile
that made it. Declared rates are the lowest layer: a model any catalog prices
is never billed at an operator's estimate.

## Refresh and the generated table

`Refresh(ctx, userHome, RefreshOptions{...})` fetches LiteLLM's JSON, honours
ETags and a minimum interval, caps the body (32 MiB by default), and rewrites
only the `external` section of `<userHome>/pricing.json`. A failed fetch keeps
the cache and says so in the outcome.

The refreshed layer uses the built-in table's keys. Each LiteLLM entry is kept
under LiteLLM's own key, and beside it go the `<backend>/<model>` rows the
generator would emit from the same data: `anthropic-vertex/claude-…` from
`vertex_ai/claude-…`, `vertex-maas/…`, `vertex/gemini-…`, `openai/…`, and the
bare row of a Gemini model listed only under a prefix. Both read one mapping,
`internal/litellm`. This matters because the refreshed layer outranks the
built-in one: keyed only LiteLLM's way, Claude Sonnet 4.5 on
`anthropic-vertex` found the refreshed bare row, Anthropic first-party's
rate, and lost Vertex's long-context tier. Refresh applies no eligibility
screen. It prices whatever model a product calls, and a real rate for an odd
model beats reporting it unpriced. A test holds the two together: given the
same LiteLLM data, every row the generator emits gets the same rate from the
refreshed layer.

The section records its `format`. One written before the qualified rows
existed (v0.13.1 and earlier) gets them derived from its LiteLLM keys when
it's loaded, so it prices right even with no network. `Refresh` also treats
it as stale whatever its age, and fetches in full without its ETag, since a
304 would keep it until LiteLLM's file next changed. A forced refresh (a
negative `MinInterval`) likewise always fetches in full. Operators don't need
to delete anything.

`pricing/builtin.go` is generated by `go run ./dev/regen-builtin-pricing`.
Membership is a rule, not a list: chat-mode, tool-calling, priced,
non-deprecated models, checked in `eligible()` and `eligibleProfile()`.
That includes a model LiteLLM lists only under a backend prefix, as it did
`gemini-4-argon` (`gemini/` and `vertex_ai/`, no bare key). Such a model gets
a bare row only from the backend whose rate a bare row carries by convention,
Vertex for Gemini and Anthropic for Claude. A model served only elsewhere, such
as a Claude model only on Vertex, gets its backend-qualified row alone. A
weekly workflow opens a pull request when LiteLLM's numbers move, with a "What
moved" section that names only the rows whose value changed.

## Introductory rates

LiteLLM's catalog has no expiry on any row, so a promotional price keeps
being re-read, with a fresh `UpdatedAt`, after it has ended. Fresh isn't the
same as true. `pricing.IntroductoryRates()` lists the promotional rates whose
end dates are known, and `IntroductoryRate.Check(rates, now)` judges a row
against one:

- before the expiry, a row still on the promotional rate is correct;
- after it, a row still on that rate **fails**;
- an entry marked `Resolved` (the increase was cancelled, as for
  claude-sonnet-5) instead pins the row *at* the former promotional rate.

core-models runs this over every builtin row a model is billed from, in CI
and in the weekly regen's verify step. A product should run it too, over
the rows it bills from, so its own build goes red when a rate it depends on
lapses, whatever core-models version it pins.

## What stays with each product

Where the pricing files live (`~/.mast`, `~/.core-agent`), whether and how
often `Refresh` runs, how a config file feeds `CfgOverride`, which models are
tier defaults, and how cost is shown are each product's. The library takes
paths and options as parameters and decides none of them.
