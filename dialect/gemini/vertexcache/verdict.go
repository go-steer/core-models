// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Originally derived from go-steer/mast@6568618:internal/vertexcacheerr (#325) and go-steer/core-agent@9d3eba89:internal/vertexcache/{gone,toosmall}.go (#902, #1067)

package vertexcache

import "strings"

// Gone reports whether err is Vertex saying the explicit cache a
// handle names is no longer there — reaped or expired — as opposed to
// an RPC merely failing. A true answer means no later call naming the
// same cache can succeed: drop the handle and create a new one.
//
// One verdict for two callers. The manager sees a dead cache on
// Caches.Update and the gemini adapter sees it on a GenerateContent
// that stamped the reference, and Vertex describes the one dead cache
// differently on each. When the two carried their own predicates, the
// manager learned the cache was gone and kept handing the name out
// while the adapter failed to recognise what it was given — mast #325
// and core-agent #902 are that bug, found independently. The observed
// shapes, all for one reaped cache:
//
//	Caches.Update    404 NOT_FOUND        Cached content <id> is not found.
//	GenerateContent  400 INVALID_ARGUMENT Cache content <id> is expired.
//	GenerateContent  404 NOT_FOUND        Not found: cached content metadata for <id>.
//
// Note "Cache content" without the d in the middle one. The genai SDK
// exposes no typed error for any of them, so this matches text, and the
// noun phrase is the discriminator: a plain NOT_FOUND (missing model,
// wrong region) and a plain 400 (a malformed request, which is what a
// bare INVALID_ARGUMENT usually is) are left alone. The status is not
// part of the match, as mast's verdict decided: it is the part that
// differs between the shapes, while the phrase names the cause in all
// of them.
func Gone(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "cache content") && !strings.Contains(s, "cached content") {
		return false
	}
	return strings.Contains(s, "not_found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "expired")
}

// IsBelowCacheMinimum reports whether err says the content Vertex was
// asked to cache is smaller than the model's minimum cacheable size:
//
//	Error 400, Message: The cached content is of 2373 tokens. The minimum
//	token count to start explicit caching is 4096., Status: INVALID_ARGUMENT
//
// That is the one Create failure time cannot fix — the cached content
// is the agent's system instruction and tools, fixed for the process's
// life — so the manager stops instead of running its retry schedule
// (core-agent #1067). It matches the reason, never the number, which
// is per-model and has moved before; pairing it with INVALID_ARGUMENT
// keeps an error that merely mentions token counts from matching.
func IsBelowCacheMinimum(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "INVALID_ARGUMENT") &&
		strings.Contains(strings.ToLower(s), "minimum token count")
}
