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
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
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
	// in tokens: tokens looked up in the cache, and tokens found there.
	// Nil when the server does not count them (an SGLang build without
	// the token counters, llama.cpp).
	Queries *float64
	Hits    *float64
	// HitRate is a hit rate the server computes itself (SGLang's
	// cache_hit_rate gauge), for servers without hit/query counters.
	HitRate *float64
	// KVUsage is the fraction of the KV cache in use, 0–1: the largest
	// across engines, since a hot engine is the one that matters.
	KVUsage *float64
}

// Window is what a server's KV cache did between two readings.
type Window struct {
	// Fleet is always true: this is the whole server's activity in the
	// window, from every client, not one session's. The field is here so
	// the fact travels with the numbers into a log or a report.
	Fleet bool
	// Start and End are the readings' times.
	Start, End time.Time
	// Queries and Hits are the counters' increase over the window, in
	// tokens. Nil when the server does not count them.
	Queries *float64
	Hits    *float64
	// HitRate is Hits/Queries when both are reported and Queries > 0;
	// otherwise the server's own rate gauge at End, when it has one.
	HitRate *float64
	// PeakKVUsage is the highest KV utilization seen at Start, End, or
	// any poll between them. Without polling it is the higher of the two
	// endpoints, which can miss a spike between them.
	PeakKVUsage *float64
	// Readings is how many snapshots the window was computed from.
	Readings int
	// Reset reports that a counter went backwards between two readings:
	// the server restarted. The increase is then summed per segment, the
	// way Prometheus's increase() does, and undercounts whatever the
	// server served between the last reading before the restart and the
	// restart itself.
	Reset bool
}

// ErrNoMetrics is returned for a server that exposes no KV-cache
// statistics (Ollama: profile.MetricsNone).
var ErrNoMetrics = errors.New("kvmetrics: this server exposes no KV-cache statistics")

// Options configures a Sampler.
type Options struct {
	// URL is the Prometheus endpoint.
	URL string
	// Format names the server's metric vocabulary.
	Format profile.MetricsFormat
	// Model, when set, reads only series labelled with that model
	// (vLLM's model_name). Empty sums every model the server serves.
	Model string
	// HTTPClient sends the scrape. Nil means a client with a 10-second
	// timeout.
	HTTPClient *http.Client
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// Sampler reads one server's metrics endpoint. Safe for concurrent use.
type Sampler struct {
	opts Options
	read func([]sample, string) Reading
}

// New builds a Sampler. It fails for profile.MetricsNone (ErrNoMetrics)
// and for an unknown or empty Format: a sampler that silently reported
// nothing would look like a server with no cache activity.
func New(opts Options) (*Sampler, error) {
	if opts.URL == "" {
		return nil, errors.New("kvmetrics: no metrics URL")
	}
	var read func([]sample, string) Reading
	switch opts.Format {
	case profile.MetricsVLLM:
		read = readVLLM
	case profile.MetricsSGLang:
		read = readSGLang
	case profile.MetricsLlamaCpp:
		read = readLlamaCpp
	case profile.MetricsNone:
		return nil, ErrNoMetrics
	case "":
		return nil, errors.New("kvmetrics: no metrics format; set the profile's metrics field (vllm, sglang or llamacpp)")
	default:
		return nil, fmt.Errorf("kvmetrics: unknown metrics format %q", opts.Format)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sampler{opts: opts, read: read}, nil
}

// FromProfile builds a Sampler for a resolved profile: its metrics_url,
// its metrics format, and its credential, so a bearer-protected endpoint
// gets the same token the API does. model narrows the reading to one
// model's series; empty reads the whole server.
func FromProfile(r *profile.Resolved, model string) (*Sampler, error) {
	if r.MetricsURL == "" {
		return nil, fmt.Errorf("kvmetrics: profile %q declares no metrics_url", r.Profile.Name)
	}
	rt := http.DefaultTransport
	if r.Credential != nil && r.Credential.Kind() != auth.None && r.Credential.Kind() != auth.APIKey {
		rt = r.Credential.BearerTransport(rt)
	}
	s, err := New(Options{
		URL:        r.MetricsURL,
		Format:     r.Profile.Metrics,
		Model:      model,
		HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: rt},
	})
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", r.Profile.Name, err)
	}
	return s, nil
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
	samples, err := parse(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Reading{}, err
	}
	r := s.read(samples, s.opts.Model)
	r.At = s.opts.Now()
	return r, nil
}

// Run is a window in progress, from Start to Stop.
type Run struct {
	s      *Sampler
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	readings []Reading
	pollErr  error
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
	run := &Run{s: s, cancel: cancel, done: make(chan struct{}), readings: []Reading{first}}
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
					run.readings = append(run.readings, r)
				}
				run.mu.Unlock()
			}
		}
	}()
	return run, nil
}

// Stop ends the polling, takes the closing reading, and returns the
// window. Safe to call once; a second call returns an error.
func (r *Run) Stop(ctx context.Context) (Window, error) {
	r.cancel()
	<-r.done
	last, err := r.s.Snapshot(ctx)
	if err != nil {
		return Window{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readings == nil {
		return Window{}, errors.New("kvmetrics: Stop called twice")
	}
	r.readings = append(r.readings, last)
	all := r.readings
	r.readings = nil
	return Between(all...), nil
}

// PollErr is the most recent failed poll, or nil. Polls that fail are
// skipped; this is for a product that wants to log them.
func (r *Run) PollErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pollErr
}

// Between computes the window spanned by readings, in time order: the
// first is the start, the last the end, and any between are polls. Fewer
// than two readings give a window with no increase.
func Between(readings ...Reading) Window {
	w := Window{Fleet: true, Readings: len(readings)}
	if len(readings) == 0 {
		return w
	}
	w.Start, w.End = readings[0].At, readings[len(readings)-1].At
	for _, r := range readings {
		if r.KVUsage != nil && (w.PeakKVUsage == nil || *r.KVUsage > *w.PeakKVUsage) {
			w.PeakKVUsage = ptr(*r.KVUsage)
		}
	}
	if len(readings) >= 2 {
		var resetQ, resetH bool
		w.Queries, resetQ = increase(readings, func(r Reading) *float64 { return r.Queries })
		w.Hits, resetH = increase(readings, func(r Reading) *float64 { return r.Hits })
		w.Reset = resetQ || resetH
	}
	if w.Queries != nil && w.Hits != nil && *w.Queries > 0 {
		w.HitRate = ptr(math.Min(1, *w.Hits / *w.Queries))
	} else if last := readings[len(readings)-1]; last.HitRate != nil {
		w.HitRate = ptr(*last.HitRate)
	}
	return w
}

// increase sums a counter's growth across consecutive readings, treating
// a drop as a restart from zero (Prometheus's increase()). Nil when any
// reading lacks the counter: an increase over a gap would be invented.
func increase(readings []Reading, get func(Reading) *float64) (*float64, bool) {
	total, reset := 0.0, false
	for i := 1; i < len(readings); i++ {
		a, b := get(readings[i-1]), get(readings[i])
		if a == nil || b == nil {
			return nil, false
		}
		if *b >= *a {
			total += *b - *a
		} else {
			total += *b
			reset = true
		}
	}
	return &total, reset
}

func ptr(v float64) *float64 { return &v }
