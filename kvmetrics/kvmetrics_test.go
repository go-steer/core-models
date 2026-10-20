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
	"fmt"
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
	tenant []string
}

func (s *serve) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	body := s.bodies[min(s.n, len(s.bodies)-1)]
	s.n++
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.tenant = append(s.tenant, r.Header.Get("X-Tenant"))
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
// doesn't serve is an error naming the models it does, not a reading of
// nils that would look like a server not reporting the metrics.
func TestModelFilter(t *testing.T) {
	ctx := context.Background()
	s := &serve{bodies: []string{fixture(t, "vllm-0.31-after.prom")}}
	got, err := sampler(t, s, profile.MetricsVLLM, "google/gemma-4-26B-A4B-it").Snapshot(ctx)
	if err != nil || got.Queries == nil || *got.Queries <= 0 {
		t.Fatalf("served model: %+v, %v", got, err)
	}
	_, err = sampler(t, s, profile.MetricsVLLM, "other/model").Snapshot(ctx)
	if err == nil || !strings.Contains(err.Error(), "other/model") || !strings.Contains(err.Error(), "google/gemma-4-26B-A4B-it") {
		t.Errorf("unserved model: %v; want an error naming it and the models present", err)
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
	if w := kvmetrics.Between(rate, rate); w.HitRate != nil || val(w.HitRateGauge) != 0.5 {
		t.Errorf("rate-only window: HitRate %v, HitRateGauge %v; want nil and the server's gauge", val(w.HitRate), val(w.HitRateGauge))
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
	r := resolve(profile.Profile{Name: "lab", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: srv.URL + "/metrics",
		Auth: auth.Config{Kind: auth.Bearer, Env: "LAB_KEY"}}, map[string]string{"LAB_KEY": "secret"})
	sm, err := kvmetrics.FromProfile(r, kvmetrics.Options{})
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
	if _, err := kvmetrics.FromProfile(ol, kvmetrics.Options{}); !errors.Is(err, kvmetrics.ErrNoMetrics) {
		t.Errorf("ollama: %v, want ErrNoMetrics", err)
	}
	nourl := resolve(profile.Profile{Name: "lab2", Extends: "vllm", BaseURL: "http://127.0.0.1:1/v1"}, nil)
	if _, err := kvmetrics.FromProfile(nourl, kvmetrics.Options{}); err == nil || !strings.Contains(err.Error(), "metrics_url") {
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

func vllmBody(engines map[string][2]float64) string {
	var b strings.Builder
	for e, qh := range engines {
		fmt.Fprintf(&b, "vllm:prefix_cache_queries_total{engine=%q,model_name=\"m\"} %v\n", e, qh[0])
		fmt.Fprintf(&b, "vllm:prefix_cache_hits_total{engine=%q,model_name=\"m\"} %v\n", e, qh[1])
	}
	return b.String()
}

func scrapeAll(t *testing.T, bodies ...string) []kvmetrics.Reading {
	t.Helper()
	s := &serve{bodies: bodies}
	sm := sampler(t, s, profile.MetricsVLLM, "")
	var out []kvmetrics.Reading
	for range bodies {
		r, err := sm.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestOneEngineRestartingIsCountedPerSeries: engine A grows by 10 while
// engine B restarts (its counter drops). The sum barely moves, but per
// series the increase is A's 10 plus B's post-restart value, and the
// window reports the reset. A total-based increase would miss it.
func TestOneEngineRestartingIsCountedPerSeries(t *testing.T) {
	rs := scrapeAll(t,
		vllmBody(map[string][2]float64{"0": {100, 50}, "1": {200, 80}}),
		vllmBody(map[string][2]float64{"0": {110, 55}, "1": {0, 0}}),
	)
	w := kvmetrics.Between(rs...)
	if !w.Reset || val(w.Queries) != 10.0 || val(w.Hits) != 5.0 {
		t.Errorf("reset %v queries %v hits %v; want reset, 10, 5", w.Reset, val(w.Queries), val(w.Hits))
	}
	// The sum still grows (310 → 330) while engine 1 restarted from 200
	// to 20: per series that is 10 + 20, and a reset.
	rs = scrapeAll(t,
		vllmBody(map[string][2]float64{"0": {100, 0}, "1": {200, 0}}),
		vllmBody(map[string][2]float64{"0": {310, 0}, "1": {20, 0}}),
	)
	if w := kvmetrics.Between(rs...); !w.Reset || val(w.Queries) != 230.0 {
		t.Errorf("sum-grows case: reset %v queries %v; want reset, 210+20", w.Reset, val(w.Queries))
	}
}

// TestSeriesAppearingAndVanishing: a series that appears mid-window
// counts from its first reading; one that disappears contributes up to
// its last.
func TestSeriesAppearingAndVanishing(t *testing.T) {
	rs := scrapeAll(t,
		vllmBody(map[string][2]float64{"0": {100, 0}}),
		vllmBody(map[string][2]float64{"0": {120, 0}, "1": {50, 0}}),
		vllmBody(map[string][2]float64{"1": {70, 0}}),
	)
	if w := kvmetrics.Between(rs...); w.Reset || val(w.Queries) != 20.0+20 {
		t.Errorf("reset %v queries %v; want no reset, 40", w.Reset, val(w.Queries))
	}
}

// TestAnOversizedScrapeFails: a body over the limit is refused, not read
// in part.
func TestAnOversizedScrapeFails(t *testing.T) {
	big := strings.Repeat("# padding padding padding padding padding padding padding\n", (16<<20)/56+2)
	_, err := sampler(t, &serve{bodies: []string{big}}, profile.MetricsVLLM, "").Snapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("oversized scrape: %v; want refused", err)
	}
}

// TestSGLangHitRateIsAveraged: the server's rate across engines is
// averaged, not maxed, so one hot engine does not overstate it.
func TestSGLangHitRateIsAveraged(t *testing.T) {
	body := "sglang:cache_hit_rate{engine=\"0\"} 0.9\nsglang:cache_hit_rate{engine=\"1\"} 0.1\n"
	r, err := sampler(t, &serve{bodies: []string{body}}, profile.MetricsSGLang, "").Snapshot(context.Background())
	if err != nil || val(r.HitRate) != 0.5 {
		t.Errorf("hit rate = %v, %v; want the mean, 0.5", val(r.HitRate), err)
	}
}

// TestAFailedClosingScrapeKeepsTheWindow: the closing scrape fails; Stop
// returns the window up to the last successful reading with EndFailed
// set, and the error.
func TestAFailedClosingScrapeKeepsTheWindow(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n.Add(1) {
		case 1:
			_, _ = w.Write([]byte(vllmBody(map[string][2]float64{"0": {100, 10}})))
		case 2:
			_, _ = w.Write([]byte(vllmBody(map[string][2]float64{"0": {150, 30}}) + "vllm:kv_cache_usage_perc 0.7\n"))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
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
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w, err := run.Stop(ctx)
	if err == nil || !w.EndFailed {
		t.Fatalf("Stop = %+v, %v; want a partial window and an error", w, err)
	}
	if val(w.Queries) != 50.0 || val(w.Hits) != 20.0 || val(w.PeakKVUsage) != 0.7 || w.Readings != 2 {
		t.Errorf("partial window = queries %v hits %v peak %v readings %d; want 50, 20, 0.7, 2", val(w.Queries), val(w.Hits), val(w.PeakKVUsage), w.Readings)
	}
}

// TestASecondStopDoesNoIO: it returns ErrStopped without scraping.
func TestASecondStopDoesNoIO(t *testing.T) {
	s := &serve{bodies: []string{vllmBody(map[string][2]float64{"0": {1, 0}})}}
	sm := sampler(t, s, profile.MetricsVLLM, "")
	ctx := context.Background()
	run, err := sm.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	before := s.n
	if _, err := run.Stop(ctx); !errors.Is(err, kvmetrics.ErrStopped) {
		t.Errorf("second Stop = %v, want ErrStopped", err)
	}
	if s.n != before {
		t.Errorf("second Stop scraped %d more time(s)", s.n-before)
	}
}

// TestRunAndBetweenAgree: a polled Run and Between over the same
// readings give the same window.
func TestRunAndBetweenAgree(t *testing.T) {
	bodies := []string{
		vllmBody(map[string][2]float64{"0": {100, 10}, "1": {50, 5}}) + "vllm:kv_cache_usage_perc 0.2\n",
		vllmBody(map[string][2]float64{"0": {140, 30}, "1": {10, 2}}) + "vllm:kv_cache_usage_perc 0.8\n",
		vllmBody(map[string][2]float64{"0": {160, 31}, "1": {40, 9}}) + "vllm:kv_cache_usage_perc 0.3\n",
	}
	want := kvmetrics.Between(scrapeAll(t, bodies...)...)
	s := &serve{bodies: bodies}
	sm := sampler(t, s, profile.MetricsVLLM, "")
	ctx := context.Background()
	run, err := sm.Start(ctx, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := s.n
		s.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got, err := run.Stop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Extra polls of the last body add no increase, so the measures
	// match whatever the poll count was.
	if val(got.Queries) != val(want.Queries) || val(got.Hits) != val(want.Hits) || val(got.PeakKVUsage) != val(want.PeakKVUsage) || got.Reset != want.Reset {
		t.Errorf("Run %+v\nBetween %+v", got, want)
	}
}

// TestCredentialsReachOnlyTheAPIsOrigin: a same-origin metrics endpoint
// gets the profile's token; another host gets none; a Google ADC token
// never goes over plain http.
func TestCredentialsReachOnlyTheAPIsOrigin(t *testing.T) {
	s := &serve{bodies: []string{"vllm:kv_cache_usage_perc 0.5\n"}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	other := httptest.NewServer(s)
	t.Cleanup(other.Close)
	scrape := func(p profile.Profile, opts profile.Options) string {
		t.Helper()
		r, err := profile.Resolve(context.Background(), p, opts)
		if err != nil {
			t.Fatal(err)
		}
		sm, err := kvmetrics.FromProfile(r, kvmetrics.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sm.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.auths[len(s.auths)-1]
	}
	env := profile.Options{Getenv: func(k string) string { return map[string]string{"K": "tok"}[k] }}
	bearer := auth.Config{Kind: auth.Bearer, Env: "K"}
	if got := scrape(profile.Profile{Name: "a", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: srv.URL + "/metrics", Auth: bearer}, env); got != "Bearer tok" {
		t.Errorf("same origin: Authorization = %q", got)
	}
	if got := scrape(profile.Profile{Name: "b", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: other.URL + "/metrics", Auth: bearer}, env); got != "" {
		t.Errorf("cross origin: Authorization = %q, want none", got)
	}
	apikey := auth.Config{Kind: auth.APIKey, Env: "K"}
	if got := scrape(profile.Profile{Name: "c", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: srv.URL + "/metrics", Auth: apikey}, env); got != "Bearer tok" {
		t.Errorf("same-origin api_key: Authorization = %q, want it sent as auth sends it", got)
	}
	adc := profile.Options{Auth: auth.Options{DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) { return staticToken("ya29"), nil }}}
	if got := scrape(profile.Profile{Name: "d", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: srv.URL + "/metrics", Auth: auth.Config{Kind: auth.GoogleADC}}, adc); got != "" {
		t.Errorf("ADC over http: Authorization = %q, want none", got)
	}
}

// TestHeadersReachOnlyTheAPIsOrigin: a profile's headers go to a
// same-origin metrics endpoint, with or without a credential, and never
// to another host.
func TestHeadersReachOnlyTheAPIsOrigin(t *testing.T) {
	s := &serve{bodies: []string{"vllm:kv_cache_usage_perc 0.5\n"}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	other := httptest.NewServer(s)
	t.Cleanup(other.Close)
	scrape := func(metricsURL string) string {
		t.Helper()
		p := profile.Profile{Name: "a", Extends: "vllm", BaseURL: srv.URL + "/v1", MetricsURL: metricsURL,
			Headers: map[string]string{"X-Tenant": "acme"}}
		r, err := profile.Resolve(context.Background(), p, profile.Options{Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatal(err)
		}
		sm, err := kvmetrics.FromProfile(r, kvmetrics.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sm.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.tenant[len(s.tenant)-1]
	}
	if got := scrape(srv.URL + "/metrics"); got != "acme" {
		t.Errorf("same origin: X-Tenant = %q, want acme", got)
	}
	if got := scrape(other.URL + "/metrics"); got != "" {
		t.Errorf("cross origin: X-Tenant = %q, want none", got)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
