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

package kvmetrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/profile"
)

// serve answers each scrape with the next exposition in bodies, repeating
// the last one, and counts scrapes and Authorization headers.
type serve struct {
	mu     sync.Mutex
	bodies []string
	n      int
	auths  []string
}

func (s *serve) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	body := s.bodies[min(s.n, len(s.bodies)-1)]
	s.n++
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()
	_, _ = w.Write([]byte(body))
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sampler(t *testing.T, s *serve, format profile.MetricsFormat, model string) *kvmetrics.Sampler {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	sm, err := kvmetrics.New(kvmetrics.Options{URL: srv.URL, Format: format, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

func val(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestVLLMWindowFromARealRecording: two scrapes of vLLM 0.31 around one
// 44-token prompt with nothing cached. The window shows 44 tokens
// queried, 0 hit, a 0 hit rate, and the peak KV utilization, all labelled
// fleet-level.
func TestVLLMWindowFromARealRecording(t *testing.T) {
	s := &serve{bodies: []string{fixture(t, "vllm-0.31-before.prom"), fixture(t, "vllm-0.31-after.prom")}}
	sm := sampler(t, s, profile.MetricsVLLM, "")
	ctx := context.Background()
	run, err := sm.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	w, err := run.Stop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Fleet || w.Readings != 2 || w.Reset {
		t.Errorf("window flags = fleet %v readings %d reset %v", w.Fleet, w.Readings, w.Reset)
	}
	if val(w.Queries) != 44.0 || val(w.Hits) != 0.0 || val(w.HitRate) != nil && *w.HitRate != 0 {
		t.Errorf("queries %v hits %v rate %v; want 44, 0, 0", val(w.Queries), val(w.Hits), val(w.HitRate))
	}
	if w.HitRate == nil {
		t.Error("hit rate is nil though both counters were reported")
	}
	if val(w.PeakKVUsage) != 0.0 {
		t.Errorf("peak KV usage = %v, want 0 (an idle server between requests)", val(w.PeakKVUsage))
	}
}

// TestModelFilter: a model the server serves narrows the reading; one it
// doesn't serve reads as not reported, never as zero.
func TestModelFilter(t *testing.T) {
	ctx := context.Background()
	s := &serve{bodies: []string{fixture(t, "vllm-0.31-after.prom")}}
	got, err := sampler(t, s, profile.MetricsVLLM, "google/gemma-4-26B-A4B-it").Snapshot(ctx)
	if err != nil || got.Queries == nil || *got.Queries <= 0 {
		t.Fatalf("served model: %+v, %v", got, err)
	}
	none, err := sampler(t, s, profile.MetricsVLLM, "other/model").Snapshot(ctx)
	if err != nil || none.Queries != nil || none.Hits != nil || none.KVUsage != nil {
		t.Errorf("unserved model read %+v, want every field nil", none)
	}
}

// TestEnginesAreSummedAndThePeakIsTheBusiest: counters add across
// engines; utilization takes the busiest engine, since averaging would
// hide a full one.
func TestEnginesAreSummedAndThePeakIsTheBusiest(t *testing.T) {
	body := `vllm:prefix_cache_queries_total{engine="0",model_name="m"} 100
vllm:prefix_cache_queries_total{engine="1",model_name="m"} 50
vllm:prefix_cache_hits_total{engine="0",model_name="m"} 40
vllm:prefix_cache_hits_total{engine="1",model_name="m"} 10
vllm:kv_cache_usage_perc{engine="0",model_name="m"} 0.2
vllm:kv_cache_usage_perc{engine="1",model_name="m"} 0.9
vllm:kv_cache_usage_perc{engine="2",model_name="m"} NaN
`
	r, err := sampler(t, &serve{bodies: []string{body}}, profile.MetricsVLLM, "").Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if val(r.Queries) != 150.0 || val(r.Hits) != 50.0 || val(r.KVUsage) != 0.9 {
		t.Errorf("reading = queries %v hits %v kv %v; want 150, 50, 0.9", val(r.Queries), val(r.Hits), val(r.KVUsage))
	}
}

// TestOlderVLLMNames: an earlier V1 build's gpu_* names still read.
func TestOlderVLLMNames(t *testing.T) {
	body := "vllm:gpu_prefix_cache_queries_total 10\nvllm:gpu_prefix_cache_hits_total 4\nvllm:gpu_cache_usage_perc 0.5\n"
	r, err := sampler(t, &serve{bodies: []string{body}}, profile.MetricsVLLM, "").Snapshot(context.Background())
	if err != nil || val(r.Queries) != 10.0 || val(r.Hits) != 4.0 || val(r.KVUsage) != 0.5 {
		t.Errorf("reading = %+v, %v", r, err)
	}
}

// TestACounterResetIsSummedPerSegment: a restart between polls drops the
// counters; the window sums growth per segment (Prometheus increase())
// and says a reset happened, instead of reporting a negative increase.
func TestACounterResetIsSummedPerSegment(t *testing.T) {
	at := func(q, h float64, i int) kvmetrics.Reading {
		return kvmetrics.Reading{At: time.Unix(int64(i), 0), Queries: &q, Hits: &h}
	}
	w := kvmetrics.Between(at(1000, 400, 0), at(1100, 450, 1), at(30, 10, 2), at(80, 30, 3))
	if !w.Reset || val(w.Queries) != 100.0+30+50 || val(w.Hits) != 50.0+10+20 {
		t.Errorf("window = reset %v queries %v hits %v; want reset, 180, 80", w.Reset, val(w.Queries), val(w.Hits))
	}
	if w.HitRate == nil || *w.HitRate < 0 || *w.HitRate > 1 {
		t.Errorf("hit rate = %v, want within [0,1]", val(w.HitRate))
	}
}

// TestUnreportedStaysNil: a server without the counters gives a window
// with nil increase and rate, not zeros (AGENTS.md rule 11); a gap in one
// reading voids the increase rather than inventing one.
func TestUnreportedStaysNil(t *testing.T) {
	q := 10.0
	w := kvmetrics.Between(kvmetrics.Reading{Queries: &q}, kvmetrics.Reading{})
	if w.Queries != nil || w.Hits != nil || w.HitRate != nil || w.PeakKVUsage != nil {
		t.Errorf("window = %+v, want every measure nil", w)
	}
}

func TestSGLangAndLlamaCpp(t *testing.T) {
	ctx := context.Background()
	sg, err := sampler(t, &serve{bodies: []string{fixture(t, "sglang-handwritten.prom")}}, profile.MetricsSGLang, "").Snapshot(ctx)
	if err != nil || val(sg.Queries) != 128000.0 || val(sg.Hits) != 96000.0 || val(sg.HitRate) != 0.75 || val(sg.KVUsage) != 0.42 {
		t.Errorf("sglang = %+v, %v", sg, err)
	}
	rate, err := sampler(t, &serve{bodies: []string{fixture(t, "sglang-handwritten-rate-only.prom")}}, profile.MetricsSGLang, "").Snapshot(ctx)
	if err != nil || rate.Queries != nil || rate.Hits != nil || val(rate.HitRate) != 0.5 || val(rate.KVUsage) != 0.3 {
		t.Errorf("sglang rate-only = %+v, %v", rate, err)
	}
	if w := kvmetrics.Between(rate, rate); val(w.HitRate) != 0.5 {
		t.Errorf("rate-only window hit rate = %v, want the server's gauge", val(w.HitRate))
	}
	lc, err := sampler(t, &serve{bodies: []string{fixture(t, "llamacpp-handwritten.prom")}}, profile.MetricsLlamaCpp, "").Snapshot(ctx)
	if err != nil || lc.Queries != nil || lc.Hits != nil || val(lc.KVUsage) != 0.25 {
		t.Errorf("llama.cpp = %+v, %v", lc, err)
	}
}

// TestPollingCatchesThePeakBetweenTheEndpoints: utilization spikes only
// in a reading between Start and Stop; with polling the window has it,
// and Stop ends the poller (the -race test run would flag a leak's
// data race on the readings).
func TestPollingCatchesThePeakBetweenTheEndpoints(t *testing.T) {
	low := "vllm:kv_cache_usage_perc 0.1\nvllm:prefix_cache_queries_total 0\nvllm:prefix_cache_hits_total 0\n"
	high := "vllm:kv_cache_usage_perc 0.95\nvllm:prefix_cache_queries_total 10\nvllm:prefix_cache_hits_total 5\n"
	var scrapes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if scrapes.Add(1) == 2 {
			_, _ = w.Write([]byte(high))
			return
		}
		_, _ = w.Write([]byte(strings.Replace(low, "queries_total 0", "queries_total 10", 1)))
	}))
	t.Cleanup(srv.Close)
	sm, err := kvmetrics.New(kvmetrics.Options{URL: srv.URL, Format: profile.MetricsVLLM})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := sm.Start(ctx, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for scrapes.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w, err := run.Stop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if val(w.PeakKVUsage) != 0.95 || w.Readings < 3 {
		t.Errorf("peak %v over %d readings; want 0.95 from the poll between the endpoints", val(w.PeakKVUsage), w.Readings)
	}
	if _, err := run.Stop(ctx); err == nil {
		t.Error("a second Stop succeeded")
	}
}

// TestFromProfile: the profile's metrics_url, format and credential are
// used, so a bearer-protected endpoint gets the profile's token; a
// profile extending a built-in inherits its format; Ollama refuses.
func TestFromProfile(t *testing.T) {
	s := &serve{bodies: []string{"vllm:kv_cache_usage_perc 0.5\n"}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	resolve := func(p profile.Profile, env map[string]string) *profile.Resolved {
		t.Helper()
		r, err := profile.Resolve(context.Background(), p, profile.Options{Getenv: func(k string) string { return env[k] }})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := resolve(profile.Profile{Name: "lab", Extends: "vllm", BaseURL: "http://127.0.0.1:1/v1", MetricsURL: srv.URL,
		Auth: auth.Config{Kind: auth.Bearer, Env: "LAB_KEY"}}, map[string]string{"LAB_KEY": "secret"})
	sm, err := kvmetrics.FromProfile(r, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sm.Snapshot(context.Background())
	if err != nil || val(got.KVUsage) != 0.5 {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
	if s.auths[0] != "Bearer secret" {
		t.Errorf("Authorization = %q, want the profile's bearer token", s.auths[0])
	}

	ol := resolve(profile.Profile{Name: "local", Extends: "ollama", MetricsURL: srv.URL}, nil)
	if _, err := kvmetrics.FromProfile(ol, ""); !errors.Is(err, kvmetrics.ErrNoMetrics) {
		t.Errorf("ollama: %v, want ErrNoMetrics", err)
	}
	nourl := resolve(profile.Profile{Name: "lab2", Extends: "vllm", BaseURL: "http://127.0.0.1:1/v1"}, nil)
	if _, err := kvmetrics.FromProfile(nourl, ""); err == nil || !strings.Contains(err.Error(), "metrics_url") {
		t.Errorf("no metrics_url: %v", err)
	}
}

func TestNewRefusesAnUnknownOrMissingFormat(t *testing.T) {
	for _, f := range []profile.MetricsFormat{"", "prometheus"} {
		if _, err := kvmetrics.New(kvmetrics.Options{URL: "http://x", Format: f}); err == nil {
			t.Errorf("format %q accepted", f)
		}
	}
}
