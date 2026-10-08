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

// Package usage is the normalized usage record a core-models adapter
// attaches to every Response, beside genai's UsageMetadata.
//
// genai's struct has no cache-write bucket and no way to say "the
// provider did not report this". Detail has both: every count is a
// pointer, nil when the provider said nothing and a pointer to zero
// when it said zero. The shape starts from mast's shipped
// internal/providers/usage.Detail (mast #352) and adds the fields a
// shared layer can populate (docs/design.md §6).
package usage

import (
	"encoding/json"

	"github.com/go-steer/core-models/llm"
)

// MetadataKey is the Response.CustomMetadata key Detail is attached
// under.
const MetadataKey = "core_models.usage"

// Detail is what a provider reported about one call beyond genai's
// usage struct. A nil count means "not reported", never zero.
type Detail struct {
	// CacheReadTokens is the prompt subset served from a provider-side
	// cache.
	CacheReadTokens *int64 `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens is the prompt subset written to a cache, across
	// every TTL. CacheWrite1hTokens is the one-hour share of it, where a
	// provider bills that differently.
	CacheWriteTokens   *int64 `json:"cache_write_tokens,omitempty"`
	CacheWrite1hTokens *int64 `json:"cache_write_1h_tokens,omitempty"`
	// ReasoningTokens and ToolUseTokens are reported counts the provider
	// broke out of its totals.
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
	ToolUseTokens   *int64 `json:"tool_use_tokens,omitempty"`

	// ServedModel is what the backend said it ran. It may not be a key
	// any price table resolves (a Vertex resource path, a deployment
	// name, a weights path); Response.ModelVersion is the priceable one.
	ServedModel string `json:"served_model,omitempty"`
	// ProviderRequestID is the id a support ticket needs.
	ProviderRequestID string `json:"provider_request_id,omitempty"`
	// Backend is the profile backend that served the call: the key a
	// price is looked up under, beside the model.
	Backend string `json:"backend,omitempty"`
	// Region is the serving region where the backend has one.
	Region string `json:"region,omitempty"`
}

// Attach puts d on resp under MetadataKey, leaving other keys alone.
func Attach(resp *llm.Response, d *Detail) {
	if resp == nil || d == nil {
		return
	}
	if resp.CustomMetadata == nil {
		resp.CustomMetadata = make(map[string]any, 1)
	}
	resp.CustomMetadata[MetadataKey] = d
}

// FromMetadata reads a Detail out of a CustomMetadata map. It accepts
// the value Attach stored, and also the map[string]any that value
// becomes after a JSON round trip through an event log — the shape a
// replay sees.
func FromMetadata(md map[string]any) (*Detail, bool) {
	switch v := md[MetadataKey].(type) {
	case *Detail:
		return v, v != nil
	case Detail:
		return &v, true
	case map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		var d Detail
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, false
		}
		return &d, true
	default:
		return nil, false
	}
}

// Int64 returns a pointer to n, for filling Detail's counts.
func Int64(n int64) *int64 { return &n }
