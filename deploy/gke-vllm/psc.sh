#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# psc.sh — publish the vLLM fixture through Private Service Connect and
# give a consumer project an internal endpoint for it.
#
# The producer (the GKE cluster running vLLM) and the consumer (where
# the agent runs) can be different projects, even different orgs. No
# VPC peering and no public endpoint: the consumer gets an internal IP
# of its own, and only the allow-listed consumer project can connect.
#
# Usage (from the repo root, with kubectl pointed at the producer
# cluster):
#
#   PRODUCER_PROJECT=<id> CONSUMER_PROJECT=<id> REGION=us-south1 \
#     deploy/gke-vllm/psc.sh
#
# Optional:
#   MODEL=gpt-oss-120b        which overlay under models/ to render
#   PRODUCER_NETWORK=default  the cluster's VPC
#   CONSUMER_NETWORK=default  where the endpoint's IP is reserved
#   CONSUMER_SUBNET=default   subnet in REGION for that IP
#   PSC_NAT_RANGE=172.16.255.0/28   must not overlap anything in the producer VPC:
#                  subnets, secondary ranges, and internal ranges (GKE
#                  pod ranges are internal ranges; list them with
#                  gcloud network-connectivity internal-ranges list)
#
# Each step checks whether its resource already exists, so re-running
# after a failure picks up where it stopped. The endpoint has global
# access enabled, so a client in any region of the consumer VPC can
# reach it, not only clients in REGION.

set -euo pipefail

: "${PRODUCER_PROJECT:?set PRODUCER_PROJECT}"
: "${CONSUMER_PROJECT:?set CONSUMER_PROJECT}"
: "${REGION:?set REGION, the region of the producer cluster}"
MODEL="${MODEL:-gpt-oss-120b}"
PRODUCER_NETWORK="${PRODUCER_NETWORK:-default}"
CONSUMER_NETWORK="${CONSUMER_NETWORK:-default}"
CONSUMER_SUBNET="${CONSUMER_SUBNET:-default}"
PSC_NAT_RANGE="${PSC_NAT_RANGE:-172.16.255.0/28}"

here="$(cd "$(dirname "$0")" && pwd)"
step() { echo; echo "▸ $*"; }

step "producer: PSC NAT subnet core-models-psc-nat ($PSC_NAT_RANGE) in $PRODUCER_PROJECT/$REGION"
if gcloud compute networks subnets describe core-models-psc-nat \
    --project "$PRODUCER_PROJECT" --region "$REGION" >/dev/null 2>&1; then
  echo "  exists"
else
  gcloud compute networks subnets create core-models-psc-nat \
    --project "$PRODUCER_PROJECT" --region "$REGION" \
    --network "$PRODUCER_NETWORK" --range "$PSC_NAT_RANGE" \
    --purpose PRIVATE_SERVICE_CONNECT
fi

step "producer: ServiceAttachment core-models/vllm, allowing $CONSUMER_PROJECT"
kubectl kustomize "$here/models/$MODEL" |
  sed "s/CONSUMER-PROJECT-ID/$CONSUMER_PROJECT/" |
  kubectl apply -f -

step "producer: waiting for the service attachment URL"
sa_url=""
for _ in $(seq 1 60); do
  sa_url="$(kubectl -n core-models get serviceattachment vllm \
    -o jsonpath='{.status.serviceAttachmentURL}' 2>/dev/null || true)"
  [[ -n "$sa_url" ]] && break
  sleep 5
done
if [[ -z "$sa_url" ]]; then
  echo "the ServiceAttachment never reported a URL; check: kubectl -n core-models describe serviceattachment vllm" >&2
  exit 1
fi
# The status URL is a full https://www.googleapis.com/... link; the
# forwarding rule wants the resource path.
sa_target="projects/${sa_url#*/projects/}"
echo "  $sa_target"

step "consumer: internal IP core-models-vllm in $CONSUMER_PROJECT/$REGION ($CONSUMER_NETWORK/$CONSUMER_SUBNET)"
if gcloud compute addresses describe core-models-vllm \
    --project "$CONSUMER_PROJECT" --region "$REGION" >/dev/null 2>&1; then
  echo "  exists"
else
  gcloud compute addresses create core-models-vllm \
    --project "$CONSUMER_PROJECT" --region "$REGION" --subnet "$CONSUMER_SUBNET"
fi

step "consumer: PSC endpoint (forwarding rule) core-models-vllm, global access on"
if gcloud compute forwarding-rules describe core-models-vllm \
    --project "$CONSUMER_PROJECT" --region "$REGION" >/dev/null 2>&1; then
  echo "  exists"
else
  gcloud compute forwarding-rules create core-models-vllm \
    --project "$CONSUMER_PROJECT" --region "$REGION" \
    --network "$CONSUMER_NETWORK" --address core-models-vllm \
    --target-service-attachment "$sa_target" \
    --allow-psc-global-access
fi

ip="$(gcloud compute addresses describe core-models-vllm \
  --project "$CONSUMER_PROJECT" --region "$REGION" --format='value(address)')"
status="$(gcloud compute forwarding-rules describe core-models-vllm \
  --project "$CONSUMER_PROJECT" --region "$REGION" --format='value(pscConnectionStatus)')"

echo
echo "endpoint: http://$ip:8000/v1   (PSC connection: $status)"
echo "check:    curl -s http://$ip:8000/health"
echo "profile:  base_url: http://$ip:8000/v1"
