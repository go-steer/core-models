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

package litellm

import "strings"

// Backend is one (backend, family) pair a built-in profile resolves to,
// paired with how LiteLLM spells the same pair.
//
// Name is core-models' name for the backend — the built-in profile's
// backend (profile.Profile.BackendName), which is what usage.Detail.Backend
// reports and both products key prices on — and it is what a price table
// emits as the "<backend>/<model>" key prefix. Using that vocabulary
// rather than LiteLLM's keeps upstream's spelling
// ("vertex_ai-anthropic_models") out of the key space, so a caller that
// has resolved a backend can build the key without a translation table.
//
// Prefix is LiteLLM's key prefix for that backend; empty means the
// backend's rates live under the unprefixed id. Note the asymmetry that
// makes this worth writing down: LiteLLM's unprefixed claude-* keys are
// ANTHROPIC first-party, while its unprefixed gemini-* keys are VERTEX.
// A bare row is therefore a mixture of two backends, which is exactly
// the ambiguity the qualified rows remove.
//
// Convention marks the backend whose rate a family's bare row carries:
// Anthropic for claude-*, Vertex for gemini-*. Discover is where LiteLLM
// lists a model of the family that has NO bare key at all, which happens:
// gemini-4-argon arrived as gemini/gemini-4-argon and
// vertex_ai/gemini-4-argon only. Empty means a model missing from the
// bare table cannot turn up on that backend (Anthropic's first-party rows
// ARE the bare table).
type Backend struct {
	Name, Prefix, Family, Discover string
	Convention                     bool
}

// Backends are the four family backends. The generator
// (dev/regen-builtin-pricing) and the pricing package's Refresh both
// read them, so the built-in table and a refreshed one spell every
// (backend, model) pair the same way.
var Backends = []Backend{
	{Name: "anthropic", Prefix: "", Family: "claude-", Convention: true},
	{Name: "anthropic-vertex", Prefix: "vertex_ai/", Family: "claude-", Discover: "vertex_ai/"},
	{Name: "gemini", Prefix: "gemini/", Family: "gemini-", Discover: "gemini/"},
	{Name: "vertex", Prefix: "", Family: "gemini-", Discover: "vertex_ai/", Convention: true},
}

// ProfileBackend is a backend reached through a core-models built-in
// profile whose models have no bare row: an id like zai-org/glm-5.2-maas
// or grok-4.7 names a model on one backend, so the (backend, model) row
// is the only row, and the context-window table (keyed on bare ids)
// does not apply.
//
// Name is the profile's backend name, Prefix is LiteLLM's key prefix
// (empty for OpenAI, whose rows are unprefixed), Provider narrows by
// litellm_provider where the prefix cannot (OpenAI's unprefixed keys sit
// beside every other vendor's), Suffix narrows to the profile's models,
// and Slash requires a publisher-qualified id.
type ProfileBackend struct {
	Name, Prefix, Provider, Suffix string
	Slash                          bool
}

// ProfileBackends are the profile backends, shared like Backends.
var ProfileBackends = []ProfileBackend{
	// Vertex AI's partner models (MaaS), served through the
	// OpenAI-compatible endpoint: vertex_ai/<publisher>/<model>-maas.
	{Name: "vertex-maas", Prefix: "vertex_ai/", Suffix: "-maas", Slash: true},
	// OpenAI first-party, through the openai-responses dialect (L2).
	{Name: "openai", Provider: "openai"},
	// xAI, through the openai-chat dialect (or Responses, opted in).
	{Name: "xai", Prefix: "xai/", Provider: "xai"},
}

// ModelOf reports the model id LiteLLM key names on b, given the
// entry's litellm_provider, or false when the key is not one of b's.
func (b ProfileBackend) ModelOf(key, provider string) (string, bool) {
	model, ok := strings.CutPrefix(key, b.Prefix)
	if !ok || !strings.HasSuffix(model, b.Suffix) || b.Slash != strings.Contains(model, "/") {
		return "", false
	}
	if b.Provider != "" && provider != b.Provider {
		return "", false
	}
	return model, true
}

// UpstreamKeys spells one model id the ways a backend's table might
// carry it, most specific first.
//
// Vertex publishes a dated model as "<base>@<date>" where the
// unprefixed Anthropic table spells it "<base>-<date>", and tags the
// undated pointer "@default". Both forms have to be tried or every
// dated id would miss and silently fall back to the bare row.
func UpstreamKeys(prefix, id string) []string {
	if prefix == "" {
		return []string{id}
	}
	keys := []string{prefix + id}
	if i := strings.LastIndex(id, "-"); i > 0 && IsDateSuffix(id[i+1:]) {
		keys = append(keys, prefix+id[:i]+"@"+id[i+1:])
	}
	return append(keys, prefix+id+"@default")
}

// BareID turns a prefixed key's model part into the id the bare table
// would use: Vertex's "<base>@<YYYYMMDD>" becomes "<base>-<YYYYMMDD>" and
// "<base>@default" becomes "<base>" (the inverse of UpstreamKeys). Any
// other "@" suffix has no bare spelling, so it reports false.
func BareID(model string) (string, bool) {
	if base, ok := strings.CutSuffix(model, "@default"); ok {
		return base, true
	}
	i := strings.LastIndex(model, "@")
	if i < 0 {
		return model, true
	}
	if IsDateSuffix(model[i+1:]) {
		return model[:i] + "-" + model[i+1:], true
	}
	return "", false
}

// IsDateSuffix reports whether s is a bare YYYYMMDD stamp.
func IsDateSuffix(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// PerMTok converts LiteLLM's per-token price to the per-million-token
// rate core-models stores, through Round6.
func PerMTok(perToken float64) float64 { return Round6(perToken * 1_000_000) }

// Round6 rounds x to 6 decimal places. Undoes binary-repr noise
// introduced when LiteLLM's per-token rates (like 0.0000001) are
// multiplied by 1M and end up as 0.09999999999999999 instead of 0.1.
// Six decimals = one-millionth-of-a-dollar per Mtok, way finer than
// any real rate we care about.
func Round6(x float64) float64 {
	const scale = 1_000_000.0
	rounded := float64(int64(x*scale+0.5)) / scale
	// Preserve exact zero (avoid returning -0 from the rounding above).
	if rounded == 0 {
		return 0
	}
	return rounded
}

// Qualify returns the rows a price table keyed in core-models' backend
// vocabulary adds to a LiteLLM catalog: each result maps the key to emit
// to the LiteLLM key whose rates it carries.
//
// priced holds every LiteLLM key the caller can price, with its
// litellm_provider. Qualify applies the generator's key rules (the
// generator additionally screens each model for agent-loop eligibility;
// Qualify does not):
//
//   - "<backend>/<id>" for every family model on every family backend
//     that lists it, from that backend's own entry (Vertex's dated
//     "@" spellings included). A model with no unprefixed Vertex entry
//     takes its Vertex rate from vertex_ai/.
//   - The bare id, for a family model LiteLLM lists only under a prefix,
//     from the family's convention backend (Vertex for gemini-*) — never
//     from another backend, because a bare claude-* row is Anthropic
//     first-party's rate by convention.
//   - "<backend>/<model>" for every profile backend's model.
//
// Keys come back lowercase, as the catalog folds every lookup.
func Qualify(priced map[string]string) map[string]string {
	out := map[string]string{}
	// Family models: every bare family key, plus every family id listed
	// only under a discovery prefix.
	ids := map[string]bool{}
	for key := range priced {
		for _, b := range Backends {
			if !strings.Contains(key, "/") {
				if strings.HasPrefix(key, b.Family) {
					ids[key] = true
				}
				continue
			}
			model, ok := strings.CutPrefix(key, b.Discover)
			if b.Discover == "" || !ok || strings.Contains(model, "/") || !strings.HasPrefix(model, b.Family) {
				continue
			}
			if id, ok := BareID(model); ok {
				ids[id] = true
			}
		}
	}
	for id := range ids {
		if _, hasBare := priced[id]; !hasBare {
			for _, b := range Backends {
				if b.Convention && b.Discover != "" && strings.HasPrefix(id, b.Family) {
					if src, ok := firstListed(priced, b.Discover, id); ok {
						out[strings.ToLower(id)] = src
					}
				}
			}
		}
		for _, b := range Backends {
			if !strings.HasPrefix(id, b.Family) {
				continue
			}
			src, ok := firstListed(priced, b.Prefix, id)
			if !ok && b.Discover != "" && b.Discover != b.Prefix {
				src, ok = firstListed(priced, b.Discover, id)
			}
			if ok {
				out[strings.ToLower(b.Name+"/"+id)] = src
			}
		}
	}
	for key, provider := range priced {
		for _, b := range ProfileBackends {
			if model, ok := b.ModelOf(key, provider); ok {
				out[strings.ToLower(b.Name+"/"+model)] = key
			}
		}
	}
	return out
}

// firstListed returns the first of id's spellings under prefix that
// priced holds.
func firstListed(priced map[string]string, prefix, id string) (string, bool) {
	for _, key := range UpstreamKeys(prefix, id) {
		if _, ok := priced[key]; ok {
			return key, true
		}
	}
	return "", false
}
