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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-models/callctx"
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

// errTransport fails every round trip with err.
type errTransport struct {
	err   error
	calls int
}

func (e *errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	e.calls++
	return nil, e.err
}

func TestTransportErrorsAreRetriedButCertificatesAreNot(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		wantCalls int
	}{
		// net/http does not export this one; it must still be retried.
		"server closed idle connection": {errors.New("http: server closed idle connection"), 3},
		"unexpected EOF":                {io.ErrUnexpectedEOF, 3},
		"unknown authority":             {&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, 1},
		"hostname mismatch":             {x509.HostnameError{Host: "x"}, 1},
		"canceled":                      {context.Canceled, 1},
	} {
		t.Run(name, func(t *testing.T) {
			base := &errTransport{err: tc.err}
			var waits []time.Duration
			req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
			_, err := policy(&waits).Transport(base).RoundTrip(req)
			if !errors.Is(err, tc.err) && err.Error() != tc.err.Error() {
				t.Errorf("err = %v, want the transport's error back", err)
			}
			if base.calls != tc.wantCalls {
				t.Errorf("%d attempts, want %d", base.calls, tc.wantCalls)
			}
		})
	}
}

// A server that accepts the request and never sends headers is the
// failure that froze an eval run for 28 minutes. The attempt is
// abandoned at HeaderTimeout and retried like a dropped connection.
func TestAnAttemptThatNeverAnswersIsAbandonedAndRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			select { // hang until the test ends or the client gives up
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	defer close(release) // before srv.Close, which waits for the hung handler

	var waits []time.Duration
	p := policy(&waits)
	p.HeaderTimeout = 50 * time.Millisecond
	ctx, rec := retry.WithRecord(context.Background())
	start := time.Now()
	resp, err := post(t, ctx, p.Transport(nil), srv.URL, "{}")
	if err != nil {
		t.Fatalf("err = %v, want the retry to succeed", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" || calls != 2 || rec.Snapshot().Retries != 1 {
		t.Errorf("body %q, %d calls, record %+v; want ok after one retry", b, calls, rec.Snapshot())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s: the hung attempt was not abandoned at the header timeout", d)
	}
}

func TestAServerThatNeverAnswersEndsInErrNoResponse(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release) // before srv.Close, which waits for the hung handlers
	var waits []time.Duration
	p := policy(&waits)
	p.HeaderTimeout = 30 * time.Millisecond
	ctx, rec := retry.WithRecord(context.Background())
	_, err := post(t, ctx, p.Transport(nil), srv.URL, "{}")
	if !errors.Is(err, retry.ErrNoResponse) {
		t.Fatalf("err = %v, want ErrNoResponse", err)
	}
	if got := rec.Snapshot(); got.Retries != 2 || got.GaveUp != retry.Budget {
		t.Errorf("record = %+v, want two retries then gave up on budget", got)
	}
}

// The deadline is for headers only. A stream that sent its headers and
// then takes longer than HeaderTimeout to finish is healthy.
func TestTheHeaderTimeoutNeverCutsABody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for i := range 5 {
			time.Sleep(40 * time.Millisecond)
			_, _ = io.WriteString(w, string(rune('a'+i)))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	var waits []time.Duration
	p := policy(&waits)
	p.HeaderTimeout = 50 * time.Millisecond // shorter than the 200ms body
	resp, err := post(t, context.Background(), p.Transport(nil), srv.URL, "{}")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "abcde" {
		t.Errorf("body = %q, %v; want the whole slow stream", b, err)
	}
}

// bareBody is the 400 the AfterSuccess tests key on.
const bareBody = `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`

func bare(w http.ResponseWriter) {
	w.WriteHeader(400)
	_, _ = io.WriteString(w, bareBody)
}

// TestAfterSuccessRetriesOnceForAServedSession is core-agent #1247 on
// the shared transport: an ambiguous 400 is retried once, and only for
// a session that has already been served.
func TestAfterSuccessRetriesOnceForAServedSession(t *testing.T) {
	var seen []string
	isBare := func(status int, body []byte) bool {
		seen = append(seen, string(body))
		return status == 400 && strings.Contains(string(body), "Request contains an invalid argument.")
	}
	served := callctx.NewPriorSuccess()
	served.Mark()

	for name, tc := range map[string]struct {
		ctx      context.Context
		steps    []func(http.ResponseWriter)
		requests int
		final    int
	}{
		"served session: retried and recovered": {
			callctx.WithPriorSuccess(context.Background(), served),
			[]func(http.ResponseWriter){bare, status(200)}, 2, 200,
		},
		"served session: retried once, not twice": {
			callctx.WithPriorSuccess(context.Background(), served),
			[]func(http.ResponseWriter){bare, bare, status(200)}, 2, 400,
		},
		"first call of a session: not retried": {
			callctx.WithPriorSuccess(context.Background(), callctx.NewPriorSuccess()),
			[]func(http.ResponseWriter){bare, status(200)}, 1, 400,
		},
		"side call: not retried": {
			callctx.AsSideCall(callctx.WithPriorSuccess(context.Background(), served), "btw"),
			[]func(http.ResponseWriter){bare, status(200)}, 1, 400,
		},
		"a 400 that says what is wrong: not retried": {
			callctx.WithPriorSuccess(context.Background(), served),
			[]func(http.ResponseWriter){status(400), status(200)}, 1, 400,
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := &script{steps: tc.steps}
			srv := httptest.NewServer(s)
			defer srv.Close()
			var waits []time.Duration
			p := policy(&waits)
			p.AfterSuccess = isBare
			resp, err := post(t, tc.ctx, p.Transport(nil), srv.URL, "{}")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(s.bodies) != tc.requests || resp.StatusCode != tc.final {
				t.Fatalf("sent %d requests, final status %d; want %d and %d", len(s.bodies), resp.StatusCode, tc.requests, tc.final)
			}
			if resp.StatusCode == 400 && len(body) == 0 {
				t.Error("the 400 handed back lost its body to the AfterSuccess check")
			}
		})
	}
	if !slices.Contains(seen, bareBody) {
		t.Errorf("AfterSuccess saw %q, want the response body among them", seen)
	}
}

// TestAfterSuccessNeverReadsASuccessfulBody: on a served session with
// AfterSuccess set, a 200 stream must reach the caller as soon as its
// headers do, not after the transport has read ahead into it.
func TestAfterSuccessNeverReadsASuccessfulBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release // the rest of the stream waits on the caller
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer srv.Close()
	defer close(release)

	served := callctx.NewPriorSuccess()
	served.Mark()
	consulted := false
	var waits []time.Duration
	p := policy(&waits)
	p.AfterSuccess = func(int, []byte) bool { consulted = true; return false }

	done := make(chan *http.Response, 1)
	go func() {
		resp, err := post(t, callctx.WithPriorSuccess(context.Background(), served), p.Transport(nil), srv.URL, "{}")
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		defer resp.Body.Close()
		buf := make([]byte, len("data: first\n\n"))
		if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "data: first\n\n" {
			t.Errorf("first chunk = %q, %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RoundTrip did not return while the stream was still open: the transport read ahead into a 200")
	}
	if consulted {
		t.Error("AfterSuccess was consulted for a 200")
	}
}
