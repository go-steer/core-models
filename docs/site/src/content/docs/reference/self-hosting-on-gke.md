---
title: Self-hosting on GKE
description: Run vLLM on a GPU in GKE and reach it from another project over Private Service Connect, using the fixture in core-models' repo.
---

core-models ships the deployment its own self-hosted tests run against:
[`deploy/gke-vllm`](https://github.com/go-steer/core-models/tree/main/deploy/gke-vllm).
It is vLLM on one GPU in a GKE cluster, published through **Private
Service Connect**. The agent can run in another project, or another
organization, and reach the server at an internal IP of its own, with no
VPC peering and no public endpoint.

## What's in it

| Piece | What it does |
|---|---|
| `base/` | Namespace, a 200 Gi PVC for weights, the vLLM Deployment (one GPU, API key from a Secret), an internal load balancer, and a `ServiceAttachment` |
| `models/gpt-oss-120b` | The default: ~63 GB MXFP4, on one 96 GB RTX PRO 6000 |
| `models/qwen3-coder-30b` | ~31 GB FP8. A fallback with room for a large KV cache |
| `models/gemma-4-26b` | ~52 GB BF16. The same weights Vertex AI serves as `gemma-4-26b-a4b-it-maas` |
| `psc.sh` | Creates the PSC NAT subnet, the ServiceAttachment allowing your consumer project, and the consumer endpoint; prints the URL |
| `house-vllm.example.json` | The matching [profile](/reference/profiles/) |

The repo's README has the step-by-step instructions.

## The shape of it

1. **Producer project:** the GKE cluster runs vLLM behind an internal load
   balancer. A ServiceAttachment publishes that load balancer to an
   allow-list of consumer projects.
2. **Consumer project:** a PSC endpoint is an internal IP in your own VPC
   that forwards to the attachment. Global access lets clients in other
   regions use it.
3. **The profile** points `base_url` at that IP. vLLM still requires its
   API key, which the profile reads from an environment variable. PSC
   decides who can connect; the key decides who can use the server.

## Lessons the fixture already encodes

- **Private nodes need egress.** Without Cloud NAT, nodes can't pull the
  vLLM image or the weights. A NAT scoped to the cluster's subnet is
  enough.
- **G4 nodes have ~47 GB of ephemeral storage**, less than a 65 GB model,
  so the weights go on a PVC. That also means a restart reloads instead of
  re-downloading.
- **A Service named `vllm` breaks vLLM's configuration.** Kubernetes
  injects `VLLM_SERVICE_HOST` and similar variables, and vLLM reads every
  `VLLM_*` variable as its own setting. The Deployment turns service links
  off.
- **A key file written by `openssl rand > file` ends in a newline**, and
  creating the Secret with `--from-file` keeps it, so every request gets
  401. Create the Secret with `--from-literal="$(…)"` instead.
- **gpt-oss on vLLM takes `tool_choice: "auto"` only.** The profile
  declares `forced_tool_choice: false`.
- **vLLM reports cached tokens per request** only with
  `--enable-prompt-tokens-details`. Its `/metrics` counters (prefix-cache
  queries and hits, KV usage) are available either way.
- **On an RTX PRO 6000**, gpt-oss runs MXFP4 through vLLM's Marlin kernels,
  not native FP4. It's correct, but not the fast path.

## What it has been tested with

See [Tested models](/reference/tested-models/). The conformance corpus
includes a recording from this deployment: `vllm-0.31-gpt-oss-120b`.
