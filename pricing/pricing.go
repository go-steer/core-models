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

// Originally derived from go-steer/mast@020004fe:internal/pricing/pricing.go (itself from go-steer/core-agent@cafe3106), with the declared layer from go-steer/core-agent@57a1500f:pkg/pricing/pricing.go

// Package pricing resolves a model's per-million-token rates across a
// layered set of sources, so usage costs stay accurate as new models ship
// and operators add overrides. It is the one catalog mast and core-agent
// share (design §9, phase L6).
//
// Layers, highest precedence first (LookupForWithSource has the exact
// order; in short, an exact key in any layer beats every prefix, and a
// higher layer beats a lower one):
//
//  1. Options.CfgOverride — the product's own per-model override (its
//     config file), keyed by model name, case-insensitive.
//  2. Options.AgentsDir/pricing.json — project-local additions.
//  3. Options.UserHome/pricing.json, section `manual` — operator-curated.
//  4. Options.UserHome/pricing.json, section `external` — fetched from
//     LiteLLM by Refresh.
//  5. builtin — the compiled-in table, generated from LiteLLM's catalog by
//     dev/regen-builtin-pricing into ./builtin.go.
//  6. Options.Declared — rates a provider profile declares
//     (models[].rates), labelled SourceDeclared. Lowest, so a model any
//     catalog prices is never billed at an operator's estimate.
//  7. longest-prefix match, layer by layer in the same order — handles
//     `gemini-3.1-pro-preview-customtools`-style suffixes.
//  8. (Rates{}, false) — rate unknown. Callers render "$—", never "$0":
//     a self-hosted model nobody priced is unpriced, and nothing here
//     invents a default rate for it.
//
// Where those files live, whether and how often Refresh runs, and how a
// product's config feeds CfgOverride are the product's (AGENTS.md
// rule 12). This package takes them as parameters.
//
// The catalog is built once (NewCatalog) and consulted on every cost
// append; lookups are read-only and lock-free.
package pricing

import (
	"strings"
	"time"
)

// Rates is the per-million-token cost for one model. CachedInputPerMTok
// is the rate applied to input tokens served from the provider's prompt
// cache (Gemini's `cachedContentTokenCount`, Anthropic's
// `cache_read_input_tokens`); a zero value means the cache-read rate
// isn't known and callers should bill cached tokens at InputPerMTok.
//
// CacheCreationInputPerMTok is the rate for input tokens that WRITE a
// cache entry — Anthropic's `cache_creation_input_tokens`, billed at a
// premium over base input rather than a discount. It holds the
// 5-minute-TTL rate (1.25x base input), LiteLLM's
// cache_creation_input_token_cost. Gemini has no equivalent bucket: its
// explicit caches bill storage per hour, not per written token, so the
// field stays zero for Gemini rows. A zero value means the cache-write
// rate isn't known and callers bill written tokens at InputPerMTok,
// which UNDERCOUNTS, so the builtin table is kept populated
// (go-steer/core-agent#263).
//
// CacheCreation1hInputPerMTok is the same bucket at Anthropic's 1-hour
// breakpoint TTL, which bills 2x base input rather than 1.25x —
// LiteLLM's cache_creation_input_token_cost_above_1hr. Two rates rather
// than one because a request may mix both TTLs and the response reports
// them separately, so there is a right answer to bill. Zero means the
// model publishes no 1-hour rate, and CostUSDWithCacheTTLs falls back to
// CacheCreationInputPerMTok (go-steer/core-agent#770).
//
// UpdatedAt records when the rate was last verified against its
// source (LiteLLM refresh time, generator run time for builtin
// entries, operator edit time for manual overrides). Zero when
// unknown. Surfaced through the products' pricing views so operators can spot stale
// entries at a glance — issue #259 called out that hand-authored
// rates drift silently, and staleness visibility is the mitigation
// baked into the "regenerate builtin from LiteLLM" workflow that
// followed.
type Rates struct {
	InputPerMTok              float64
	CachedInputPerMTok        float64
	CacheCreationInputPerMTok float64
	// CacheCreation1hInputPerMTok is the write rate for a 1-hour
	// breakpoint (2x base input on Anthropic, against 1.25x for the
	// 5-minute one CacheCreationInputPerMTok holds). Zero when the
	// source publishes none; see CostUSDWithCacheTTLs for the fallback.
	CacheCreation1hInputPerMTok float64
	OutputPerMTok               float64
	UpdatedAt                   time.Time

	// LongContext, when set, replaces these rates for a call whose prompt
	// exceeds its threshold: OpenAI's 272K cliff, Gemini's 200K, some
	// Claude models' 100K. Nil for a model with one price at every size.
	LongContext *LongContext
}

// LongContext is a model's above-threshold price. LiteLLM publishes
// absolute rates per bucket ("input_cost_per_token_above_272k_tokens",
// …), not a multiplier, and the buckets don't move together —
// gemini-2.5-pro's cache reads double above 200K while its output goes
// 1.5x — so the rates are carried as published.
//
// A call is long when its whole prompt (uncached + cache reads + cache
// writes) is above ThresholdTokens; then every token of it, output
// included, bills at these rates, which is how the providers charge.
type LongContext struct {
	ThresholdTokens             int     `json:"threshold_tokens"`
	InputPerMTok                float64 `json:"input_per_mtok,omitempty"`
	CachedInputPerMTok          float64 `json:"cached_input_per_mtok,omitempty"`
	CacheCreationInputPerMTok   float64 `json:"cache_creation_input_per_mtok,omitempty"`
	CacheCreation1hInputPerMTok float64 `json:"cache_creation_1h_input_per_mtok,omitempty"`
	OutputPerMTok               float64 `json:"output_per_mtok,omitempty"`
}

// At returns the rates that bill a call whose prompt is promptTokens
// long: r itself, or r with its LongContext rates in force when the
// prompt is above the threshold. The result has no LongContext of its
// own.
//
// A bucket the long-context row leaves at zero is not free: it falls
// back to the base rate scaled by the input multiplier (long input over
// base input), so an unpublished above-threshold cache rate tracks the
// published input increase instead of silently staying at the cheaper
// base price. Output falls back to the base output rate unscaled, since
// output and input move independently.
func (r Rates) At(promptTokens int) Rates {
	lc := r.LongContext
	r.LongContext = nil
	if lc == nil || lc.ThresholdTokens <= 0 || promptTokens <= lc.ThresholdTokens || lc.InputPerMTok <= 0 {
		return r
	}
	mult := 1.0
	if r.InputPerMTok > 0 {
		mult = lc.InputPerMTok / r.InputPerMTok
	}
	pick := func(long, base float64, scaled bool) float64 {
		switch {
		case long > 0:
			return long
		case scaled:
			return base * mult
		default:
			return base
		}
	}
	out := r
	out.InputPerMTok = lc.InputPerMTok
	out.CachedInputPerMTok = pick(lc.CachedInputPerMTok, r.CachedInputPerMTok, true)
	out.CacheCreationInputPerMTok = pick(lc.CacheCreationInputPerMTok, r.CacheCreationInputPerMTok, true)
	out.CacheCreation1hInputPerMTok = pick(lc.CacheCreation1hInputPerMTok, r.CacheCreation1hInputPerMTok, true)
	out.OutputPerMTok = pick(lc.OutputPerMTok, r.OutputPerMTok, false)
	return out
}

// IsZero reports whether the rates carry no useful pricing.
// Used by callers to distinguish "free model" from "rate unknown" —
// only the latter should render "$—". CachedInputPerMTok isn't part
// of this check: a row that carries only a cache rate but no base
// input/output rates is still "unpriced" in the useful sense.
func (r Rates) IsZero() bool { return r.InputPerMTok == 0 && r.OutputPerMTok == 0 }

// CostUSD returns the dollar cost of (input, output) tokens at r.
// Treats every input token as uncached — see CostUSDWithCache for the
// cached-vs-uncached split.
func (r Rates) CostUSD(inputTokens, outputTokens int) float64 {
	return r.CostUSDWithCacheTTLs(inputTokens, 0, 0, 0, outputTokens)
}

// CostUSDWithCache returns the dollar cost with cache-hit tokens billed
// at CachedInputPerMTok. When CachedInputPerMTok is zero (rate unknown)
// cached tokens fall back to InputPerMTok — no silent free-riding.
//
// Providers that also report cache-WRITE tokens should call
// CostUSDWithCacheWrites instead; this signature folds them into the
// uncached bucket, which undercounts (go-steer/core-agent#263).
func (r Rates) CostUSDWithCache(uncachedInputTokens, cachedInputTokens, outputTokens int) float64 {
	return r.CostUSDWithCacheWrites(uncachedInputTokens, cachedInputTokens, 0, outputTokens)
}

// CostUSDWithCacheWrites is CostUSDWithCache plus the cache-write
// bucket: tokens that created a cache entry this turn, billed at
// CacheCreationInputPerMTok.
//
// The three input buckets are mutually exclusive and must not overlap —
// pass uncached = total prompt - cache reads - cache writes. Unknown
// rates fall back to InputPerMTok for both cache buckets rather than to
// zero, so a missing catalog entry degrades to the old (understated)
// number instead of billing cached or written tokens as free.
//
// Every write token is billed at the 5-minute rate. Callers that know
// how the write bucket split across TTLs should use
// CostUSDWithCacheTTLs; this signature has no bucket for the 1-hour
// share, so it understates a 1h write by the gap between the two rates.
func (r Rates) CostUSDWithCacheWrites(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, outputTokens int) float64 {
	return r.CostUSDWithCacheTTLs(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, 0, outputTokens)
}

// CostUSDWithCacheTTLs is CostUSDWithCacheWrites with the write bucket
// split by breakpoint TTL. cacheWrite1hTokens is a SUBSET of
// cacheWriteTokens — the share Anthropic reports under
// usage.cache_creation.ephemeral_1h_input_tokens — and is billed at
// CacheCreation1hInputPerMTok; the remainder goes at the 5-minute rate.
//
// A 1-hour rate of zero falls back to the 5-minute rate rather than to
// base input, as core-agent's pricing does (core-agent #770): a model
// that publishes one write rate and not the other is far likelier to be
// missing a catalog field than to bill 1h writes at base, so the nearer
// neighbour is the better estimate.
func (r Rates) CostUSDWithCacheTTLs(uncachedInputTokens, cacheReadTokens, cacheWriteTokens, cacheWrite1hTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	// Long context is decided on the whole prompt, so every bucket and
	// the output bill at the same tier.
	r = r.At(uncachedInputTokens + cacheReadTokens + cacheWriteTokens)
	readRate := r.CachedInputPerMTok
	if readRate == 0 {
		readRate = r.InputPerMTok
	}
	writeRate := r.CacheCreationInputPerMTok
	if writeRate == 0 {
		writeRate = r.InputPerMTok
	}
	write1hRate := r.CacheCreation1hInputPerMTok
	if write1hRate == 0 {
		write1hRate = writeRate
	}
	if cacheWrite1hTokens > cacheWriteTokens {
		cacheWrite1hTokens = cacheWriteTokens
	}
	if cacheWrite1hTokens < 0 {
		cacheWrite1hTokens = 0
	}
	return (float64(uncachedInputTokens)/million)*r.InputPerMTok +
		(float64(cacheReadTokens)/million)*readRate +
		(float64(cacheWriteTokens-cacheWrite1hTokens)/million)*writeRate +
		(float64(cacheWrite1hTokens)/million)*write1hRate +
		(float64(outputTokens)/million)*r.OutputPerMTok
}

// Catalog is the merged view of all pricing sources, queried by
// model name. Construct with NewCatalog; consult with Lookup.
//
// Layers are stored separately so the daily LiteLLM refresh can rewrite
// the external slice without touching the others, and so the
// precedence chain stays explicit (no "where did this rate come
// from" mystery).
type Catalog struct {
	// Sources, highest precedence first. Each map is lowercased on
	// insert so lookups are case-insensitive without per-call
	// allocations.
	cfgOverride map[string]Rates // Options.CfgOverride
	projectFile map[string]Rates // <AgentsDir>/pricing.json
	userManual  map[string]Rates // <UserHome>/pricing.json "manual"
	userExt     map[string]Rates // <UserHome>/pricing.json "external"
	builtin     map[string]Rates // compiled-in fallback
	declared    map[string]Rates // provider profiles' declared rates

	// isolated holds the backends priced only from their own rows: the
	// built-in profiles' isolated backends, adjusted by Options.Profiles.
	isolated map[string]bool

	layers []layerWithSource // the maps above, in precedence order
}

// Layer source names surfaced via LookupWithSource and the products'
// pricing views. Stable strings — operators grep for them, docs
// reference them. Don't rename without a deprecation cycle.
const (
	SourceCfgOverride  = "cfg-override"
	SourceProjectFile  = "project-file"
	SourceUserManual   = "user-manual"
	SourceUserExternal = "user-external"
	SourceBuiltin      = "builtin"
	// SourceDeclared is a rate a provider profile declares for a model
	// (models[].rates): the operator's own price for a model no catalog
	// covers, such as a self-hosted one. core-agent's spelling, kept.
	SourceDeclared = "profile-declared"
)

// Lookup returns the resolved rates for modelID plus a found flag.
// !found means the caller should treat the cost as unknown ($—)
// rather than zero.
//
// Lookup is LookupFor with no backend; see LookupForWithSource for the
// resolution order.
func (c *Catalog) Lookup(modelID string) (Rates, bool) {
	r, _, ok := c.LookupWithSource(modelID)
	return r, ok
}

// LookupFor returns the rates for a model as served by a named backend
// — a core-models backend name, as profile.Profile.BackendName and
// usage.Detail.Backend report it: "anthropic", "anthropic-vertex",
// "gemini", "vertex", "vertex-maas", "openai", "xai", or a profile's
// own — and is the lookup a caller should prefer whenever it knows which
// backend will bill the tokens.
//
// A rate is a property of the (backend, model) pair. The same
// claude-opus-5 is reachable first-party and through Vertex; the same
// gemini-3.7-flash through the Developer API and through Vertex. Lookup
// cannot tell those apart, and the bare table it consults is itself a
// mixture — LiteLLM's unprefixed claude-* rates are first-party
// Anthropic while its unprefixed gemini-* rates are Vertex.
//
// Both the "<backend>/<model>" key and the bare model id are consulted,
// layer by layer, so an operator's bare override still beats a builtin
// qualified row; see LookupForWithSource for the exact order. The bare
// fallback is what makes this safe to call everywhere — a pair no table
// qualifies resolves exactly as Lookup would, rather than reporting the
// model unpriced and dropping the session's cost to zero.
//
// An empty backend is exactly Lookup, which is the right behavior for
// an offline fake or a model whose backend could not be resolved.
func (c *Catalog) LookupFor(backend, modelID string) (Rates, bool) {
	r, _, ok := c.LookupForWithSource(backend, modelID)
	return r, ok
}

// LookupForWithSource is LookupFor plus the layer that served the rate.
// See LookupWithSource for the source-name vocabulary.
//
// Resolution runs in two phases, and the first hit wins:
//
//  1. Exact. For each layer in precedence order (cfg-override,
//     project-file, user-manual, user-external, builtin, declared):
//     the qualified key "<backend>/<model>", then the bare model id.
//  2. Prefix, only when no layer holds an exact key. For each layer in
//     the same order: the longest key that prefixes the qualified key
//     (and is longer than "<backend>/"), then the longest key that
//     prefixes the bare id.
//
// Layer precedence therefore outranks qualification: an operator's
// bare override beats a builtin qualified row, because the operator
// wrote it and the builtin did not. Exactness outranks both: a dated
// id with its own bare row is never billed at a qualified family
// prefix. Within one layer, qualified beats bare at the same phase,
// which is the point of a qualified key.
//
// An isolated backend (a built-in or Options.Profiles profile with
// pricing "isolated", such as sap-ai-core) skips every bare key, in
// every layer and both phases: only "<backend>/<model>" rows price it.
// A gateway that bills in its own units would otherwise be billed at
// the first-party list price of every model it also offers.
func (c *Catalog) LookupForWithSource(backend, modelID string) (Rates, string, bool) {
	if c == nil {
		return Rates{}, "", false
	}
	bare := strings.ToLower(strings.TrimSpace(modelID))
	if bare == "" {
		return Rates{}, "", false
	}
	var qual string
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend != "" {
		qual = backend + "/" + bare
	}
	useBare := !c.isIsolated(backend)
	layers := c.layersWithSource()
	for _, ls := range layers {
		if qual != "" {
			if r, ok := ls.layer[qual]; ok {
				return r, ls.source, true
			}
		}
		if useBare {
			if r, ok := ls.layer[bare]; ok {
				return r, ls.source, true
			}
		}
	}
	for _, ls := range layers {
		if qual != "" {
			if r, ok := longestPrefix(ls.layer, qual, len(backend)+1); ok {
				return r, ls.source, true
			}
		}
		if useBare {
			if r, ok := longestPrefix(ls.layer, bare, 0); ok {
				return r, ls.source, true
			}
		}
	}
	return Rates{}, "", false
}

// LookupWithSource is Lookup + the name of the catalog layer that
// served the rate (SourceCfgOverride / SourceProjectFile /
// SourceUserManual / SourceUserExternal / SourceBuiltin /
// SourceDeclared). Empty source string when !ok. Products show it so
// operators can spot stale builtin rates that should have been
// overridden by a fresh LiteLLM refresh but weren't — the visibility
// that #259 asked for.
//
// Resolution is LookupForWithSource's with no qualified key: an exact
// match by layer precedence, then the longest prefix within the first
// layer that holds one.
func (c *Catalog) LookupWithSource(modelID string) (Rates, string, bool) {
	return c.LookupForWithSource("", modelID)
}

// longestPrefix returns the row whose key is the longest prefix of key
// that is longer than minLen bytes. It probes the map once per
// candidate length instead of scanning every key, so a miss costs at
// most len(key) lookups however large the layer is.
func longestPrefix(layer map[string]Rates, key string, minLen int) (Rates, bool) {
	if len(layer) == 0 {
		return Rates{}, false
	}
	for i := len(key); i > minLen; i-- {
		if r, ok := layer[key[:i]]; ok {
			return r, true
		}
	}
	return Rates{}, false
}

// layerWithSource pairs one layer map with its source-name string.
type layerWithSource struct {
	layer  map[string]Rates
	source string
}

// layersWithSource is the precedence-ordered pairing consulted by
// LookupForWithSource — highest precedence first. NewCatalog builds it
// once; a Catalog assembled field by field (tests) gets it built per
// call.
func (c *Catalog) layersWithSource() []layerWithSource {
	if c.layers != nil {
		return c.layers
	}
	return c.buildLayers()
}

func (c *Catalog) buildLayers() []layerWithSource {
	return []layerWithSource{
		{c.cfgOverride, SourceCfgOverride},
		{c.projectFile, SourceProjectFile},
		{c.userManual, SourceUserManual},
		{c.userExt, SourceUserExternal},
		{c.builtin, SourceBuiltin},
		{c.declared, SourceDeclared},
	}
}

// CountByLayer reports how many model entries each layer holds.
// Surfaced by the products' pricing views and useful for tests that
// want to assert the expected number of rows landed in each layer.
type CountByLayer struct {
	CfgOverride  int
	ProjectFile  int
	UserManual   int
	UserExternal int
	Builtin      int
	Declared     int
}

// Counts returns per-layer entry counts.
func (c *Catalog) Counts() CountByLayer {
	if c == nil {
		return CountByLayer{}
	}
	return CountByLayer{
		CfgOverride:  len(c.cfgOverride),
		ProjectFile:  len(c.projectFile),
		UserManual:   len(c.userManual),
		UserExternal: len(c.userExt),
		Builtin:      len(c.builtin),
		Declared:     len(c.declared),
	}
}
