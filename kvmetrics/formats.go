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

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/go-steer/core-models/profile"
)

// A reader turns a parsed exposition into a Reading for one server's
// metric vocabulary. Each metric is looked up under its current name
// first and older names after. A metric no name matches stays nil.
type reader struct {
	queries, hits []string // counters, in tokens
	hitRate       []string // a server-computed rate gauge, averaged
	kvUsage       []string // a 0–1 utilization gauge, the busiest series
	// modelLabeled says the server labels series with the model, so
	// Options.Model can narrow a reading.
	modelLabeled bool
}

// modelLabel is the label vLLM and SGLang put the served model on.
const modelLabel = "model_name"

func readerFor(f profile.MetricsFormat) (reader, error) {
	switch f {
	case profile.MetricsVLLM:
		// vLLM V1 (verified on 0.31): the prefix-cache counters are in
		// tokens, and kv_cache_usage_perc is a 0–1 fraction despite its
		// name. The gpu_* names are earlier V1 releases'.
		return reader{
			queries:      []string{"vllm:prefix_cache_queries_total", "vllm:gpu_prefix_cache_queries_total", "vllm:gpu_prefix_cache_queries"},
			hits:         []string{"vllm:prefix_cache_hits_total", "vllm:gpu_prefix_cache_hits_total", "vllm:gpu_prefix_cache_hits"},
			kvUsage:      []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"},
			modelLabeled: true,
		}, nil
	case profile.MetricsSGLang:
		// SGLang, from its documentation (not yet verified against a live
		// server). Every prompt token is looked up in SGLang's radix
		// cache, so prompt_tokens_total is the query count and
		// cached_tokens_total the hits, where the build exports them;
		// older builds have only the cache_hit_rate gauge. token_usage is
		// the KV pool in use, 0–1. SGLang has used both prefixes.
		return reader{
			queries:      []string{"sglang:prompt_tokens_total", "sglang_prompt_tokens_total"},
			hits:         []string{"sglang:cached_tokens_total", "sglang_cached_tokens_total"},
			hitRate:      []string{"sglang:cache_hit_rate", "sglang_cache_hit_rate"},
			kvUsage:      []string{"sglang:token_usage", "sglang_token_usage"},
			modelLabeled: true,
		}, nil
	case profile.MetricsLlamaCpp:
		// llama.cpp server's --metrics, from its documentation (not yet
		// verified against a live server): utilization, but no
		// prefix-cache hit counters.
		return reader{kvUsage: []string{"llamacpp:kv_cache_usage_ratio"}}, nil
	case profile.MetricsNone:
		return reader{}, ErrNoMetrics
	case "":
		return reader{}, fmt.Errorf("kvmetrics: no metrics format; set the profile's metrics field (vllm, sglang or llamacpp)")
	}
	return reader{}, fmt.Errorf("kvmetrics: unknown metrics format %q", f)
}

func (rd reader) read(ss []sample, model string) Reading {
	if !rd.modelLabeled {
		model = ""
	}
	var r Reading
	r.Queries, r.queries = counter(ss, model, rd.queries)
	r.Hits, r.hits = counter(ss, model, rd.hits)
	r.HitRate = mean(ss, model, rd.hitRate)
	r.KVUsage = peak(ss, model, rd.kvUsage)
	return r
}

// checkModel refuses a model filter that matches nothing the endpoint
// has: a reading of all nils would look like a server that does not
// report the metrics, when it reports them for other models. It names
// the models that are present.
func (rd reader) checkModel(ss []sample, model string) error {
	if model == "" || !rd.modelLabeled {
		return nil
	}
	known := slices.Concat(rd.queries, rd.hits, rd.hitRate, rd.kvUsage)
	present := map[string]bool{}
	for _, s := range ss {
		if slices.Contains(known, s.name) {
			if m, ok := s.labels[modelLabel]; ok {
				present[m] = true
			}
		}
	}
	if len(present) == 0 || present[model] {
		return nil
	}
	names := make([]string, 0, len(present))
	for m := range present {
		names = append(names, m)
	}
	sort.Strings(names)
	return fmt.Errorf("kvmetrics: the metrics endpoint has no series for model %q (it has %s)", model, strings.Join(names, ", "))
}

// counter returns the total of the first name that has any series,
// optionally only those labelled with model, and the value per series
// keyed by name and full label set. Nil when no name has a series.
func counter(ss []sample, model string, names []string) (*float64, map[string]float64) {
	for _, n := range names {
		total, series := 0.0, map[string]float64{}
		for _, s := range ss {
			if s.name == n && matches(s, model) {
				total += s.value
				series[seriesKey(s)] = s.value
			}
		}
		if len(series) > 0 {
			return &total, series
		}
	}
	return nil, nil
}

// mean averages the first name's series: a server-computed rate across
// engines. A maximum would overstate it, and no series exposes a weight
// to average by.
func mean(ss []sample, model string, names []string) *float64 {
	for _, n := range names {
		total, count := 0.0, 0
		for _, s := range ss {
			if s.name == n && matches(s, model) {
				total += s.value
				count++
			}
		}
		if count > 0 {
			return ptr(total / float64(count))
		}
	}
	return nil
}

// peak is the largest value among the first name's series: for a
// utilization gauge across engines, the busiest one.
func peak(ss []sample, model string, names []string) *float64 {
	for _, n := range names {
		var best *float64
		for _, s := range ss {
			if s.name == n && matches(s, model) && (best == nil || s.value > *best) {
				best = ptr(s.value)
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

// seriesKey identifies a series by name and full label set.
func seriesKey(s sample) string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	for _, k := range keys {
		fmt.Fprintf(&b, "\x00%s=%s", k, s.labels[k])
	}
	return b.String()
}
