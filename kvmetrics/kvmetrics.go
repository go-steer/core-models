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

// Package kvmetrics samples a self-hosted server's KV-cache statistics
// from its Prometheus endpoint (docs/design.md §8): prefix-cache queries
// and hits, the hit rate, and KV-cache utilization.
//
// It is opt-in, and it provides facts, not policy: the library reads the
// server, and each product decides when to sample (mast at session start
// and end, core-agent per /stats). A library embed with no metrics
// endpoint never builds one.
//
// What it reports is FLEET-LEVEL and TIME-CORRELATED. A server's
// counters cover every request it served in the window, from every
// client, so a Window says what the server did while a session ran, not
// what the session did. It is never attributed to a request or a
// specialist, and never enters usage.Detail or a cost figure.
//
// Every count is a pointer, as in usage.Detail (AGENTS.md rule 11): a
// metric the server does not expose is nil, "not reported", never 0.
package kvmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"
)

// Reading is one snapshot of a server's metrics, summed over its engines
// (and over its models, unless Options.Model narrows it).
type Reading struct {
	// At is when the snapshot was taken.
	At time.Time
	// Queries and Hits are the server's cumulative prefix-cache counters,
	// in tokens, summed across series: tokens looked up in the cache, and
	// tokens found there. Nil when the server does not count them (an
	// SGLang build without the token counters, llama.cpp).
	Queries *float64
	Hits    *float64
	// HitRate is the hit rate the server computes itself (SGLang's
	// cache_hit_rate gauge), averaged across its series, for servers
	// without hit/query counters. Instantaneous, not a window's rate.
	HitRate *float64
	// KVUsage is the fraction of the KV cache in use, 0–1: the largest
	// across engines, since a hot engine is the one that matters.
	KVUsage *float64

	// queries and hits are the counters per series (name and full label
	// set), which is what a window's increase is computed from: a total
	// summed across engines cannot tell one engine restarting from the
	// others slowing down.
	queries, hits map[string]float64
}

// Window is what a server's KV cache did between two readings.
type Window struct {
	// Fleet is always true: this is the whole server's activity in the
	// window, from every client, not one session's. The field is here so
	// the fact travels with the numbers into a log or a report.
	Fleet bool
	// Start and End are the first and last readings' times.
	Start, End time.Time
	// Queries and Hits are the counters' increase over the window, in
	// tokens, computed per series and summed. Nil when fewer than two
	// readings carried the counter.
	Queries *float64
	Hits    *float64
	// HitRate is Hits/Queries over the window, when both are reported and
	// Queries > 0. Nil otherwise: it is never filled from a gauge.
	HitRate *float64
	// HitRateGauge is the server's own hit-rate gauge at the last reading
	// that had one (Reading.HitRate): instantaneous, for servers with no
	// counters. Kept apart from HitRate so a consumer cannot mistake one
	// for the other.
	HitRateGauge *float64
	// PeakKVUsage is the highest KV utilization seen at Start, End, or
	// any poll between them. Without polling it is the higher of the two
	// endpoints, which can miss a spike between them.
	PeakKVUsage *float64
	// Readings is how many snapshots the window was computed from.
	Readings int
	// Reset reports that some series' counter went backwards between two
	// readings: that engine restarted. Its increase is then summed per
	// segment, the way Prometheus's increase() does, and undercounts what
	// it served between its last reading before the restart and the
	// restart itself.
	Reset bool
	// EndFailed reports that the closing scrape failed, so the window
	// ends at the last successful poll (End is that poll's time).
	EndFailed bool
}

// ErrNoMetrics is returned for a server that exposes no KV-cache
// statistics (Ollama: profile.MetricsNone).
var ErrNoMetrics = errors.New("kvmetrics: this server exposes no KV-cache statistics")

// maxScrape bounds one exposition. A scrape over it fails rather than
// being read in part: a truncated exposition would report a smaller
// number as if it were the whole.
const maxScrape = 16 << 20

// Options configures a Sampler.
type Options struct {
	// URL is the Prometheus endpoint. FromProfile takes it from the
	// profile.
	URL string
	// Format names the server's metric vocabulary. FromProfile takes it
	// from the profile.
	Format profile.MetricsFormat
	// Model, when set, reads only series labelled with that model
	// (vLLM's and SGLang's model_name). Empty sums every model the server
	// serves. A model the endpoint has no series for is an error, not an
	// empty reading.
	Model string
	// HTTPClient sends the scrape, as given: FromProfile attaches no
	// credential to a caller's client. Nil means a client with a
	// 10-second timeout.
	HTTPClient *http.Client
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// Sampler reads one server's metrics endpoint. Safe for concurrent use.
type Sampler struct {
	opts   Options
	reader reader
}

// New builds a Sampler. It fails for profile.MetricsNone (ErrNoMetrics)
// and for an unknown or empty Format: a sampler that silently reported
// nothing would look like a server with no cache activity.
func New(opts Options) (*Sampler, error) {
	if opts.URL == "" {
		return nil, errors.New("kvmetrics: no metrics URL")
	}
	rd, err := readerFor(opts.Format)
	if err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sampler{opts: opts, reader: rd}, nil
}

// FromProfile builds a Sampler for a resolved profile's metrics_url and
// metrics format. opts supplies the rest (Model, HTTPClient, Now); its
// URL and Format are ignored.
//
// The profile's credential is attached only when metrics_url has the
// same origin (scheme, host and port) as the profile's base_url, and a
// Google ADC token only over https: a metrics endpoint on another host
// gets no token meant for the API. It is sent the way package auth sends
// it to the API (BearerTransport: "Authorization: Bearer", for an API key
// too). An endpoint that needs a different credential takes a caller's
// opts.HTTPClient, to which nothing is added.
func FromProfile(r *profile.Resolved, opts Options) (*Sampler, error) {
	if r.MetricsURL == "" {
		return nil, fmt.Errorf("kvmetrics: profile %q declares no metrics_url", r.Profile.Name)
	}
	opts.URL, opts.Format = r.MetricsURL, r.Profile.Metrics
	if opts.HTTPClient == nil {
		rt := http.DefaultTransport
		if attachCredential(r) {
			rt = r.Credential.BearerTransport(rt)
		}
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: rt}
	}
	s, err := New(opts)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", r.Profile.Name, err)
	}
	return s, nil
}

// attachCredential reports whether a profile's credential may go to its
// metrics endpoint: same origin as the API, and https for a Google ADC
// token.
func attachCredential(r *profile.Resolved) bool {
	if r.Credential == nil || r.Credential.Kind() == auth.None {
		return false
	}
	m, err1 := url.Parse(r.MetricsURL)
	b, err2 := url.Parse(r.BaseURL)
	if err1 != nil || err2 != nil || !strings.EqualFold(m.Scheme, b.Scheme) || !strings.EqualFold(m.Host, b.Host) {
		return false
	}
	return r.Credential.Kind() != auth.GoogleADC || strings.EqualFold(m.Scheme, "https")
}

// Snapshot scrapes the endpoint once.
func (s *Sampler) Snapshot(ctx context.Context) (Reading, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.URL, nil)
	if err != nil {
		return Reading{}, fmt.Errorf("kvmetrics: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return Reading{}, fmt.Errorf("kvmetrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Reading{}, fmt.Errorf("kvmetrics: %s answered %s", s.opts.URL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrape+1))
	if err != nil {
		return Reading{}, fmt.Errorf("kvmetrics: %w", err)
	}
	if len(body) > maxScrape {
		return Reading{}, fmt.Errorf("kvmetrics: %s sent more than %d bytes; refusing a partial read", s.opts.URL, maxScrape)
	}
	samples, err := parse(bytes.NewReader(body))
	if err != nil {
		return Reading{}, err
	}
	if err := s.reader.checkModel(samples, s.opts.Model); err != nil {
		return Reading{}, err
	}
	r := s.reader.read(samples, s.opts.Model)
	r.At = s.opts.Now()
	return r, nil
}

// Run is a window in progress, from Start to Stop. It keeps a running
// fold of its readings, not the readings, so its memory does not grow
// with the session.
type Run struct {
	s      *Sampler
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	acc     accumulator
	stopped bool
	pollErr error
}

// Start takes the opening reading and, when interval > 0, polls the
// endpoint every interval until Stop, so the window's peak KV
// utilization and any counter reset between the endpoints are seen. A
// failed poll is skipped, not fatal: one missed scrape should not void
// the window. The polling goroutine ends when Stop is called or ctx is
// done.
//
// Start/Stop rather than two Snapshot calls because a peak needs the
// readings between the two points, and only a running poller has them.
// A product that schedules its own scrapes can call Snapshot and Between
// instead.
func (s *Sampler) Start(ctx context.Context, interval time.Duration) (*Run, error) {
	first, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(ctx)
	run := &Run{s: s, cancel: cancel, done: make(chan struct{})}
	run.acc.add(first)
	if interval <= 0 {
		close(run.done)
		return run, nil
	}
	go func() {
		defer close(run.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				r, err := s.Snapshot(pctx)
				run.mu.Lock()
				if err != nil {
					if pctx.Err() == nil {
						run.pollErr = err
					}
				} else {
					run.acc.add(r)
				}
				run.mu.Unlock()
			}
		}
	}()
	return run, nil
}

// ErrStopped is returned by a second Stop.
var ErrStopped = errors.New("kvmetrics: Stop called twice")

// Stop ends the polling, takes the closing reading, and returns the
// window. If the closing scrape fails, Stop still returns the window up
// to the last successful reading, with EndFailed set, together with the
// scrape's error: the window is usable whenever it is non-zero. A second
// Stop returns ErrStopped without scraping.
func (r *Run) Stop(ctx context.Context) (Window, error) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return Window{}, ErrStopped
	}
	r.stopped = true
	r.mu.Unlock()

	r.cancel()
	<-r.done
	last, err := r.s.Snapshot(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		w := r.acc.window()
		w.EndFailed = true
		return w, fmt.Errorf("kvmetrics: closing scrape failed; the window ends at the last successful reading: %w", err)
	}
	r.acc.add(last)
	return r.acc.window(), nil
}

// PollErr is the most recent failed poll, or nil. Polls that fail are
// skipped; this is for a product that wants to log them.
func (r *Run) PollErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pollErr
}

// Between computes the window spanned by readings, in time order: the
// first is the start, the last the end, and any between are polls. It
// folds them exactly as a Run does, so the two agree.
func Between(readings ...Reading) Window {
	var a accumulator
	for _, r := range readings {
		a.add(r)
	}
	return a.window()
}

// accumulator folds readings one at a time into what a Window needs, in
// constant memory per series.
type accumulator struct {
	n          int
	start, end time.Time
	peak       *float64
	gauge      *float64
	q, h       counterFold
}

func (a *accumulator) add(r Reading) {
	if a.n == 0 {
		a.start = r.At
	}
	a.n++
	a.end = r.At
	if r.KVUsage != nil && (a.peak == nil || *r.KVUsage > *a.peak) {
		a.peak = ptr(*r.KVUsage)
	}
	if r.HitRate != nil {
		a.gauge = ptr(*r.HitRate)
	}
	a.q.add(seriesOf(r.queries, r.Queries))
	a.h.add(seriesOf(r.hits, r.Hits))
}

func (a *accumulator) window() Window {
	w := Window{Fleet: true, Readings: a.n, Start: a.start, End: a.end, PeakKVUsage: a.peak, HitRateGauge: a.gauge}
	w.Queries, w.Hits = a.q.increase(), a.h.increase()
	w.Reset = a.q.reset || a.h.reset
	if w.Queries != nil && w.Hits != nil && *w.Queries > 0 {
		w.HitRate = ptr(math.Min(1, *w.Hits / *w.Queries))
	}
	return w
}

// seriesOf returns a reading's per-series counter values, or, for a
// Reading built by hand with only a total, that total as one series.
func seriesOf(series map[string]float64, total *float64) map[string]float64 {
	if series != nil {
		return series
	}
	if total != nil {
		return map[string]float64{"": *total}
	}
	return nil
}

// counterFold sums one counter family's increase per series, the way
// Prometheus's increase() does: a drop is a restart from zero. A series
// that first appears mid-window counts from its first reading; one that
// disappears contributes up to its last. The increase is nil until the
// family has been seen in two readings: one reading has no window.
type counterFold struct {
	seen  int
	last  map[string]float64
	inc   float64
	reset bool
}

func (c *counterFold) add(series map[string]float64) {
	if series == nil {
		return
	}
	c.seen++
	if c.last == nil {
		c.last = make(map[string]float64, len(series))
	}
	for k, v := range series {
		if a, ok := c.last[k]; ok {
			if v >= a {
				c.inc += v - a
			} else {
				c.inc += v
				c.reset = true
			}
		}
		c.last[k] = v
	}
}

func (c *counterFold) increase() *float64 {
	if c.seen < 2 {
		return nil
	}
	return ptr(c.inc)
}

func ptr(v float64) *float64 { return &v }
