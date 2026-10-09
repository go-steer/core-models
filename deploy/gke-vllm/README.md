# vLLM on GKE, reached over Private Service Connect

A self-hosted model server for core-models' `openai-chat` dialect: vLLM
on one GPU in a GKE cluster, published through Private Service Connect.
The agent can therefore run in another project, or another organization,
and reach the server at an internal IP of its own, with no VPC peering
and no public endpoint.

This is the deployment core-models' conformance corpus and tool-calling
parity runs use (`testdata/conformance/vllm-*`).

```
base/                  namespace, PVC, Deployment, internal LB Service, ServiceAttachment
models/gpt-oss-120b/   ~63 GB MXFP4: the default
models/qwen3-coder-30b/  ~31 GB FP8: fallback, room for a large KV cache
models/gemma-4-26b/    ~52 GB BF16: same weights as Vertex AI's gemma-4-26b-a4b-it-maas
psc.sh                 PSC NAT subnet, ServiceAttachment, consumer endpoint
house-vllm.example.json  the matching provider profile
```

Every overlay targets one 96 GB GPU (RTX PRO 6000 Blackwell, a G4 node)
and pins `vllm/vllm-openai:v0.31.0`.

## Deploy

With `kubectl` pointed at the producer cluster:

1. **The API key.** It's created out of band and never committed. Use
   `--from-literal` with command substitution: a key written to a file by
   `openssl rand > file` ends in a newline, and `--from-file` copies the
   newline into the key, so every request fails with 401.

   ```sh
   kubectl apply -f base/namespace.yaml
   kubectl -n core-models create secret generic vllm-api-key \
     --from-literal=api-key="$(openssl rand -hex 24)"
   ```

2. **The server.** Pick a model overlay. The first start downloads the
   weights to the PVC, which takes minutes, then loads them onto the GPU.

   ```sh
   kubectl apply -k models/gpt-oss-120b
   ```

   The ServiceAttachment in the overlay carries a placeholder consumer
   project. It stays unready until step 3 sets the real one.

3. **Private Service Connect.** This creates the PSC NAT subnet and the
   ServiceAttachment allowing your consumer project, then reserves the
   consumer's internal IP and creates the endpoint. It prints the endpoint
   URL.

   ```sh
   PRODUCER_PROJECT=<producer> CONSUMER_PROJECT=<consumer> REGION=<region> ./psc.sh
   ```

   Run it as one line: variables on a line of their own are not passed to
   the script. `PSC_NAT_RANGE` (default `172.16.255.0/28`) must not overlap
   anything in the producer VPC, including GKE pod ranges, which are
   *internal ranges* (`gcloud network-connectivity internal-ranges list`),
   not subnets.

4. **The profile.** Copy `house-vllm.example.json`, set the endpoint IP,
   and export `HOUSE_VLLM_API_KEY`. mast reads profiles from
   `.agents/providers/*.yaml`; the same fields work in YAML.

## Things this fixture already handles

- **Private nodes need egress** for Docker Hub and Hugging Face. Without
  Cloud NAT the pod never pulls. The deployment this was built on used a
  NAT scoped to the cluster's subnet only.
- **G4 nodes have ~47 GB of ephemeral storage**, too little for a 65 GB
  model. The weights go on a 200 Gi PVC (`dynamic-rwo`, which picks
  Hyperdisk on G4) and survive restarts.
- **A Service named `vllm` injects `VLLM_*` environment variables**, which
  vLLM reads as its own configuration. The Deployment sets
  `enableServiceLinks: false`.
- **gpt-oss on vLLM takes `tool_choice: "auto"` only**, so the profile
  declares `forced_tool_choice: false` for it.
- **Cached tokens are reported per request** only with
  `--enable-prompt-tokens-details`, which every overlay sets. Server-wide
  prefix-cache counters are on `/metrics` regardless.
- **On the RTX PRO 6000 (SM120)**, gpt-oss runs MXFP4 through vLLM's
  Marlin kernels, not native FP4. It works, but it isn't the fast path.

## Clean up

```sh
kubectl delete namespace core-models     # Deployment, Service, ServiceAttachment, PVC and its disk
gcloud compute forwarding-rules delete core-models-vllm --project <consumer> --region <region>
gcloud compute addresses delete core-models-vllm --project <consumer> --region <region>
gcloud compute networks subnets delete core-models-psc-nat --project <producer> --region <region>
```
