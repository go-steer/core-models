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

# live-canary.sh — one leg of the monthly live canary
# (.github/workflows/live-canary.yml): the conformance scenarios, run
# live against one first-party provider, so a provider that changes its
# wire format under a recorded corpus is noticed within a month rather
# than by a product's user. NOT a presubmit, and all.sh does not run it:
# it needs a key and spends money (four calls, a few cents at most).
#
# Usage: dev/ci/live-canary.sh <leg>
#
# Legs: openai-chat openai-responses xai-chat xai-responses anthropic gemini
#
# A leg whose key is not in the environment is skipped (exit 0) with a
# GitHub notice, so the workflow works with any subset of the secrets
# configured. The `go test -v` output is written to
# $CANARY_LOG (default: a temp file) for the workflow's failure report.

set -euo pipefail
cd "$(dirname "$0")/../.."

leg="${1:?usage: live-canary.sh <leg>}"
dialect=""
case "$leg" in
  openai-chat)      profile=openai;    model=gpt-5-mini;               key=OPENAI_API_KEY;    dialect=openai-chat ;;
  openai-responses) profile=openai;    model=gpt-5-mini;               key=OPENAI_API_KEY ;;
  xai-chat)         profile=xai;       model=grok-4.20-0309-reasoning; key=XAI_API_KEY ;;
  xai-responses)    profile=xai;       model=grok-4.20-0309-reasoning; key=XAI_API_KEY;       dialect=openai-responses ;;
  anthropic)        profile=anthropic; model=claude-haiku-4-5;         key=ANTHROPIC_API_KEY ;;
  gemini)           profile=gemini;    model=gemini-3.6-flash;         key=GEMINI_API_KEY ;;
  *) echo "live-canary.sh: unknown leg ${leg}" >&2; exit 2 ;;
esac

if [[ -z "${!key:-}" ]]; then
  echo "::notice title=live canary: ${leg} skipped::${key} is not set; add it as a repository secret to run this leg"
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export CORE_MODELS_LIVE_PROFILE="$profile" CORE_MODELS_LIVE_MODEL="$model"
unset CORE_MODELS_LIVE_RECORD CORE_MODELS_LIVE_BASE_URL CORE_MODELS_LIVE_PROFILE_FILE
if [[ -n "$dialect" ]]; then
  # The leg exercises a dialect the built-in profile does not default
  # to, through a profile that extends it — the same shape the
  # xai-responses corpus was recorded with.
  CORE_MODELS_LIVE_PROFILE="canary"
  printf '[{"name":"%s","extends":"%s","dialect":"%s"}]\n' \
    "$CORE_MODELS_LIVE_PROFILE" "$profile" "$dialect" >"${work}/profiles.json"
  export CORE_MODELS_LIVE_PROFILE_FILE="${work}/profiles.json"
fi

log="${CANARY_LOG:-${work}/live.log}"
echo "live canary: ${leg} (profile ${CORE_MODELS_LIVE_PROFILE}, model ${model})"
# -count=1: a cached pass would be a canary that never calls anything.
go test -tags live -count=1 -timeout 10m -run 'TestLive$' -v . 2>&1 | tee "$log"
