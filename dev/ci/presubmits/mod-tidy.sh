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

# mod-tidy.sh — presubmit: `go mod tidy` is a no-op in every module.
#
# Compares file CONTENT before and after rather than `git diff`
# (mast's and core-agent's shape), so uncommitted go.mod edits during
# local development are neither flagged nor destroyed: the tree is
# left exactly as found.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml); run
# dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
. "$(dirname "$0")/../../tools/common.sh"

tidy_one() {
  local tmp
  tmp=$(mktemp -d)
  cp go.mod go.sum "$tmp/"
  go mod tidy
  if ! cmp -s go.mod "$tmp/go.mod" || ! cmp -s go.sum "$tmp/go.sum"; then
    diff -u "$tmp/go.mod" go.mod >&2 || true
    diff -u "$tmp/go.sum" go.sum >&2 || true
    cp "$tmp/go.mod" "$tmp/go.sum" .
    rm -rf "$tmp"
    echo "not tidy: run 'go mod tidy' in $(pwd) and commit the result" >&2
    return 1
  fi
  rm -rf "$tmp"
}

each_module tidy_one
echo "mod-tidy: OK"
