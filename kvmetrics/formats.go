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

package kvmetrics

import "math"

// The per-server readers turn a parsed exposition into a Reading. Each
// metric is looked up under its current name first and older names
// after, and summed across engines (and models, unless one is asked
// for). A metric no name matches stays nil.

// modelLabel is the label vLLM and SGLang put the served model on.
const modelLabel = "model_name"

// readVLLM reads vLLM's V1 metrics (verified on vLLM 0.31): the
// prefix-cache counters are in tokens, and kv_cache_usage_perc is a
// 0–1 fraction despite its name. The gpu_* names are earlier V1
// releases'.
func readVLLM(ss []sample, model string) Reading {
	return Reading{
		Queries: sum(ss, model, "vllm:prefix_cache_queries_total", "vllm:gpu_prefix_cache_queries_total", "vllm:gpu_prefix_cache_queries"),
		Hits:    sum(ss, model, "vllm:prefix_cache_hits_total", "vllm:gpu_prefix_cache_hits_total", "vllm:gpu_prefix_cache_hits"),
		KVUsage: peak(ss, model, "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"),
	}
}

// readSGLang reads SGLang's metrics, from its documentation (not yet
// verified against a live server). Every prompt token is looked up in
// SGLang's radix cache, so prompt_tokens_total is the query count and
// cached_tokens_total the hits, where the build exports them; older
// builds have only the cache_hit_rate gauge. token_usage is the KV pool
// in use, 0–1. SGLang has used both "sglang:" and "sglang_" prefixes.
func readSGLang(ss []sample, model string) Reading {
	return Reading{
		Queries: sum(ss, model, "sglang:prompt_tokens_total", "sglang_prompt_tokens_total"),
		Hits:    sum(ss, model, "sglang:cached_tokens_total", "sglang_cached_tokens_total"),
		HitRate: peak(ss, model, "sglang:cache_hit_rate", "sglang_cache_hit_rate"),
		KVUsage: peak(ss, model, "sglang:token_usage", "sglang_token_usage"),
	}
}

// readLlamaCpp reads llama.cpp server's --metrics endpoint, from its
// documentation (not yet verified against a live server). It reports
// KV-cache utilization but no prefix-cache hit counters, so Queries and
// Hits stay nil.
func readLlamaCpp(ss []sample, _ string) Reading {
	return Reading{
		KVUsage: peak(ss, "", "llamacpp:kv_cache_usage_ratio"),
	}
}

// sum adds every series of the first name that has any, optionally only
// those labelled with model. Nil when no name has a series.
func sum(ss []sample, model string, names ...string) *float64 {
	for _, n := range names {
		total, found := 0.0, false
		for _, s := range ss {
			if s.name == n && matches(s, model) {
				total += s.value
				found = true
			}
		}
		if found {
			return &total
		}
	}
	return nil
}

// peak is the largest value among the first name's series: for a
// utilization gauge across engines, the busiest one.
func peak(ss []sample, model string, names ...string) *float64 {
	for _, n := range names {
		var best *float64
		for _, s := range ss {
			if s.name == n && matches(s, model) && (best == nil || s.value > *best) {
				v := s.value
				best = &v
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// matches reports whether s counts toward a reading: labelled with
// model when one is asked for, and a finite value. A NaN or infinite
// sample (a server's "no data yet") would poison a sum or a peak.
func matches(s sample, model string) bool {
	if math.IsNaN(s.value) || math.IsInf(s.value, 0) {
		return false
	}
	return model == "" || s.labels[modelLabel] == model
}
