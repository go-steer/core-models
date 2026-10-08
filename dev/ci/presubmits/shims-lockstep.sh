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

# shims-lockstep.sh — presubmit: adkv1 is adkv2 with the import path
# changed, and nothing else.
#
# The two shims are the same field copies over two ADK majors whose
# model types are identical in shape. A fix made to one and not the
# other is how they would start to disagree about what a response
# contains, so the check is a diff rather than a review habit. Edit
# adkv2, then regenerate adkv1:
#
#   dev/ci/presubmits/shims-lockstep.sh --fix
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml); run
# dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

render() {
  sed -e 's|google.golang.org/adk/v2/|google.golang.org/adk/|g' -e 's|adkv2|adkv1|g' "adkv2/$1"
}

status=0
for f in adkv2.go adkv2_test.go; do
  v1="adkv1/${f/adkv2/adkv1}"
  if [[ "${1:-}" == "--fix" ]]; then
    render "$f" >"$v1"
  elif ! diff -u "$v1" <(render "$f") >&2; then
    echo "FAIL: $v1 has drifted from adkv2/$f (diff above; regenerate with --fix)." >&2
    status=1
  fi
done
if ((status == 0)); then
  echo "shims-lockstep: OK"
fi
exit "${status}"
