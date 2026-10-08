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

# core-no-adk.sh — presubmit: the core module never imports ADK.
#
# Decision D2 (docs/design.md §2): the core depends on genai and
# vendor SDKs only, and ADK enters through the adkv1/adkv2 modules.
# If any package of the core module reaches an ADK major — directly
# or through a dependency — every consumer gets that major in its
# build, and core-agent (ADK v1) and mast (ADK v2) cannot both import
# the same core. go build would not notice; this does.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml); run
# dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

deps="$(go list -deps -test ./...)"
if adk="$(grep -E '^google\.golang\.org/adk(/|$)' <<<"${deps}")"; then
  echo "FAIL: the core module reaches ADK (D2):" >&2
  sed 's/^/  - /' <<<"${adk}" >&2
  echo "Move the ADK-facing code into adkv1/ and adkv2/." >&2
  exit 1
fi
echo "core-no-adk: OK"
