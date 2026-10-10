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

// Originally derived from go-steer/mast@6568618:internal/providers/gemini/projection.go (projectGrounding)

package gemini

import "google.golang.org/genai"

// AuthorGoogleSearch is the author both products give the synthetic
// session events that surface Google Search evidence. Stable, so event
// log consumers can filter on it.
const AuthorGoogleSearch = "gemini/google_search"

// GroundingEvidence renders what Google Search grounding did as lines
// for an audit trail: one "query: …" per search the model issued, then
// one per grounded web source ("Title — URI", or the URI alone).
// Duplicates are dropped — Vertex repeats a query or source across
// search rounds — and a source with no URI, which names nothing anyone
// can follow, is skipped. Nil when there is no evidence, the common
// case.
//
// This is the ADK-free half of both products' GroundingProjection.
// The other half wraps an ADK session.Service and appends one event per
// line under AuthorGoogleSearch; it lives with the product (or a shim),
// because session events are ADK's.
func GroundingEvidence(gm *genai.GroundingMetadata) []string {
	if gm == nil || (len(gm.WebSearchQueries) == 0 && len(gm.GroundingChunks) == 0) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, q := range gm.WebSearchQueries {
		if q != "" {
			add("query: " + q)
		}
	}
	for _, ch := range gm.GroundingChunks {
		if ch == nil || ch.Web == nil || ch.Web.URI == "" {
			continue
		}
		if ch.Web.Title != "" {
			add(ch.Web.Title + " — " + ch.Web.URI)
		} else {
			add(ch.Web.URI)
		}
	}
	return out
}
