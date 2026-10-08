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

package retry_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-models/retry"
)

// script serves a fixed sequence of responses, one per request, and
// records the bodies it was sent.
type script struct {
	mu     sync.Mutex
	steps  []func(w http.ResponseWriter)
	bodies []string
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	n := len(s.bodies)
	s.bodies = append(s.bodies, string(b))
	s.mu.Unlock()
	if n >= len(s.steps) {
		http.Error(w, "script exhausted", http.StatusTeapot)
		return
	}
	s.steps[n](w)
}

func status(code int, headers ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, http.StatusText(code))
	}
}

// policy returns Default with a recording, instant Sleep and no jitter.
func policy(waits *[]time.Duration) retry.Policy {
	p := retry.Default()
	p.Jitter = 0
	p.Sleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
	return p
}

func post(t *testing.T, ctx context.Context, rt http.RoundTripper, url, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return rt.RoundTrip(req)
}

func TestRetriesHonorTheServersHeaders(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		headers []string
		want    time.Duration
	}{
		"retry-after-ms":               {[]string{"retry-after-ms", "1500"}, 1500 * time.Millisecond},
		"fractional retry-after-ms":    {[]string{"retry-after-ms", "250.5"}, 250500 * time.Microsecond},
		"retry-after seconds":          {[]string{"retry-after", "3"}, 3 * time.Second},
		"retry-after HTTP date":        {[]string{"retry-after", now.Add(7 * time.Second).Format(http.TimeFormat)}, 7 * time.Second},
		"retry-after-ms wins":          {[]string{"retry-after-ms", "100", "retry-after", "9"}, 100 * time.Millisecond},
		"no header: initial backoff":   {nil, time.Second},
		"unparseable: initial backoff": {[]string{"retry-after", "soon"}, time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			s := &script{steps: []func(http.ResponseWriter){status(429, tc.headers...), status(200)}}
			srv := httptest.NewServer(s)
			defer srv.Close()
			var waits []time.Duration
			p := policy(&waits)
			p.Now = func() time.Time { return now }

			ctx, rec := retry.WithRecord(context.Background())
			resp, err := post(t, ctx, p.Transport(nil), srv.URL, "{}")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200 after one retry", resp.StatusCode)
			}
			if len(waits) != 1 || waits[0] != tc.want {
				t.Errorf("waits = %v, want [%v]", waits, tc.want)
			}
			got := rec.Snapshot()
			if got.Retries != 1 || got.LastStatus != 429 || got.GaveUp != "" || got.WaitedMS != tc.want.Milliseconds() {
				t.Errorf("record = %+v", got)
			}
		})
	}
}

func TestBackoffGrowsAndTheBudgetEnds(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(503), status(503), status(503), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	var waits []time.Duration
	p := policy(&waits) // MaxRetries 2

	ctx, rec := retry.WithRecord(context.Background())
	resp, err := post(t, ctx, p.Transport(nil), srv.URL, "{}")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want the last 503 handed back", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "Service Unavailable" {
		t.Errorf("returned body = %q: the response handed back must be readable", b)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; len(waits) != 2 || waits[0] != want[0] || waits[1] != want[1] {
		t.Errorf("waits = %v, want %v", waits, want)
	}
	if got := rec.Snapshot(); got.Retries != 2 || got.GaveUp != retry.Budget {
		t.Errorf("record = %+v, want 2 retries and gave up on budget", got)
	}
	if len(s.bodies) != 3 {
		t.Errorf("server saw %d requests, want 3", len(s.bodies))
	}
}

func TestEveryAttemptCarriesTheWholeBody(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(500), status(502), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	var waits []time.Duration
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	resp, err := post(t, context.Background(), policy(&waits).Transport(nil), srv.URL, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for i, got := range s.bodies {
		if got != body {
			t.Errorf("attempt %d sent %q, want the original body", i+1, got)
		}
	}
}

func TestWhatIsNotRetried(t *testing.T) {
	for name, step := range map[string]func(http.ResponseWriter){
		"400":                         status(400),
		"401":                         status(401),
		"404":                         status(404),
		"429 with should-retry false": status(429, "x-should-retry", "false"),
	} {
		t.Run(name, func(t *testing.T) {
			s := &script{steps: []func(http.ResponseWriter){step, status(200)}}
			srv := httptest.NewServer(s)
			defer srv.Close()
			var waits []time.Duration
			ctx, rec := retry.WithRecord(context.Background())
			resp, err := post(t, ctx, policy(&waits).Transport(nil), srv.URL, "{}")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if len(s.bodies) != 1 || len(waits) != 0 {
				t.Errorf("sent %d requests and waited %v; want one request, no wait", len(s.bodies), waits)
			}
			if got := rec.Snapshot(); got != (retry.Snapshot{}) {
				t.Errorf("record = %+v, want empty", got)
			}
		})
	}
}

func TestShouldRetryTrueOverridesTheStatus(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(409, "x-should-retry", "true"), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	var waits []time.Duration
	resp, err := post(t, context.Background(), policy(&waits).Transport(nil), srv.URL, "{}")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200: x-should-retry: true makes a 409 retryable", resp.StatusCode)
	}
}

func TestAServerThatAsksTooMuchGetsItsAnswerBack(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(429, "retry-after", "120"), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	var waits []time.Duration
	ctx, rec := retry.WithRecord(context.Background())
	resp, err := post(t, ctx, policy(&waits).Transport(nil), srv.URL, "{}")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || len(waits) != 0 {
		t.Errorf("status %d after waits %v; want the 429 back, unwaited", resp.StatusCode, waits)
	}
	if got := rec.Snapshot(); got.GaveUp != retry.HeaderDelay || got.LastStatus != 429 || got.Retries != 0 {
		t.Errorf("record = %+v", got)
	}
}

// onceReader is a body http.NewRequest cannot rewind (no GetBody).
type onceReader struct{ io.Reader }

func TestABodyThatCannotBeResentIsNotRetried(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(503), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	var waits []time.Duration
	ctx, rec := retry.WithRecord(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, onceReader{bytes.NewReader([]byte("{}"))})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := policy(&waits).Transport(nil).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || len(s.bodies) != 1 {
		t.Errorf("status %d after %d requests; want the 503 back after one", resp.StatusCode, len(s.bodies))
	}
	if got := rec.Snapshot(); got.GaveUp != retry.NotReplayable {
		t.Errorf("record = %+v", got)
	}
}

func TestCancelDuringTheWaitStops(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(503), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ctx, rec := retry.WithRecord(ctx)
	p := retry.Default()
	p.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	resp, err := post(t, ctx, p.Transport(nil), srv.URL, "{}")
	if err == nil {
		resp.Body.Close()
		t.Fatal("no error after the context was canceled mid-wait")
	}
	if len(s.bodies) != 1 {
		t.Errorf("server saw %d requests, want 1", len(s.bodies))
	}
	if got := rec.Snapshot(); got.GaveUp != retry.Canceled {
		t.Errorf("record = %+v", got)
	}
}

func TestAConnectionThatDiesIsRetried(t *testing.T) {
	// The first connection is accepted and closed before any response;
	// the second is served normally.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })}
	srv.ConnState = func(c net.Conn, st http.ConnState) {
		if st != http.StateNew {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		conns++
		if conns == 1 {
			_ = c.Close()
		}
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	var waits []time.Duration
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	ctx, rec := retry.WithRecord(context.Background())
	resp, err := post(t, ctx, policy(&waits).Transport(tr), "http://"+ln.Addr().String(), "{}")
	if err != nil {
		t.Fatalf("err = %v, want the retry to reach the server", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || rec.Snapshot().Retries != 1 {
		t.Errorf("status %d, record %+v; want 200 after one retry", resp.StatusCode, rec.Snapshot())
	}
}

func TestZeroRetriesIsAPassThrough(t *testing.T) {
	s := &script{steps: []func(http.ResponseWriter){status(429), status(200)}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	var waits []time.Duration
	p := policy(&waits)
	p.MaxRetries = 0
	ctx, rec := retry.WithRecord(context.Background())
	resp, err := post(t, ctx, p.Transport(nil), srv.URL, "{}")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || len(s.bodies) != 1 || rec.Snapshot() != (retry.Snapshot{}) {
		t.Errorf("status %d, %d requests, record %+v; want a plain pass-through", resp.StatusCode, len(s.bodies), rec.Snapshot())
	}
}

func TestJitterStaysInsideItsBand(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		s := &script{steps: []func(http.ResponseWriter){status(503), status(200)}}
		srv := httptest.NewServer(s)
		var waits []time.Duration
		p := policy(&waits)
		p.Jitter = 0.25
		p.Rand = func() float64 { return r }
		resp, err := post(t, context.Background(), p.Transport(nil), srv.URL, "{}")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		if len(waits) != 1 || waits[0] < 750*time.Millisecond || waits[0] >= 1250*time.Millisecond {
			t.Errorf("rand %v: waits = %v, want one wait in [750ms, 1250ms)", r, waits)
		}
	}
}

func TestSnapshotOfNoRecord(t *testing.T) {
	var r *retry.Record
	if r.Snapshot() != (retry.Snapshot{}) {
		t.Error("a nil Record is not empty")
	}
}
