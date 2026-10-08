---
title: Modules and versions
description: The Go modules core-models publishes, their import paths, how they are tagged, and the dependency floors they impose.
---

## Import paths

| Module | Import |
|---|---|
| Core | `github.com/go-steer/core-models` (packages `llm`, `usage`, `callctx`, `retry`, `auth`, `profile`) |
| ADK v1 adapter | `github.com/go-steer/core-models/adkv1` |
| ADK v2 adapter | `github.com/go-steer/core-models/adkv2` |

A consumer requires the core module and the adapter for its ADK major,
never both adapters.

## Tags

Each module is tagged separately, Go's convention for nested modules:
- `v0.x.y` for the core;
- `adkv1/v0.x.y` and `adkv2/v0.x.y` for the adapters.

core-models is pre-1.0 and its API may change between minor versions.

## Dependency floors

A library's requirements become the minimum versions its consumers get.
The core module therefore requires the **lowest** genai version either
product uses, so importing core-models never upgrades a product's genai
underneath its own tests.

Google credentials come from `cloud.google.com/go/auth`, at the version
genai already requires, so the core adds no module for them.

Exceptions are made for security fixes. The core requires gRPC 1.83.1
for GO-2026-6348, a version both products already use.
