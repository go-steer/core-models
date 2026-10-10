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

// Package retry retries model calls at the HTTP layer, where a
// provider's rate-limit headers are still visible.
//
// # Why the HTTP layer
//
// mast's modelretry and core-agent's RetryPolicy both retry at
// model.LLM, the one interface every provider arrives through. That
// level cannot see Retry-After: genai's APIError carries no response,
// so on the Gemini path the header is gone before a model.LLM decorator
// runs (mast's modelretry documents this). Each vendor SDK also has its
// own opinion — anthropic-sdk-go retries twice, genai never — so the
// same workload got different resilience depending on which model it
// named.
//
// core-models' adapters therefore turn their SDK's own retries off and
// install Transport on the HTTP client instead. One policy, one header
// rule, every provider. A product's model.LLM-level policy stays where
// it is and now sees only what this layer gave up on (docs/design.md
// Q7).
//
// # What is retried
//
// A response whose status is 408, 429, 500, 502, 503 or 504, and a
// request that failed before any response arrived (a refused or dropped
// connection, a timeout, a DNS failure — anything but a certificate
// error), unless the server sent
// x-should-retry: false (OpenAI and Anthropic send it). x-should-retry:
// true makes any status retryable. A canceled context is never
// retried.
//
// Only what happens before the response body is handed back can be
// retried: a stream that breaks halfway through has already delivered
// tokens to the caller, and replaying the request would deliver them
// twice. That failure surfaces to the adapter as an error, as it should.
//
// # How long it waits
//
// retry-after-ms if present (OpenAI), else retry-after as seconds or an
// HTTP date, else exponential backoff with jitter. A server that asks
// for longer than Policy.MaxHeaderDelay gets its response handed back
// rather than a shorter wait: retrying sooner than asked earns another
// 429 and spends budget, and a wait that long is the product's call,
// not a transport's.
//
// The approach — prefer retry-after-ms, accept an HTTP date, bound the
// delay — follows charmbracelet/fantasy's retry.go (mast model-support
// §10.2); no code is taken from it.
package retry

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-steer/core-models/callctx"
)

// Policy configures Transport. The zero value is not useful; start
// from Default.
type Policy struct {
	// MaxRetries is how many times one request may be re-sent. Zero
	// disables retrying.
	MaxRetries int
	// InitialBackoff is the first wait when the server gives no header;
	// each later wait multiplies it by BackoffFactor.
	InitialBackoff time.Duration
	BackoffFactor  float64
	// MaxBackoff caps a computed backoff.
	MaxBackoff time.Duration
	// MaxHeaderDelay is the longest server-requested wait honored. A
	// longer request ends retrying and returns the response.
	MaxHeaderDelay time.Duration
	// HeaderTimeout bounds how long one attempt may wait for response
	// headers. Expiry abandons the attempt and counts as a transport
	// failure, so it is retried like a dropped connection. It never
	// limits the body: a stream that has sent its headers may run as long
	// as it generates. A non-streaming request receives headers only once
	// the whole answer exists, so set it above the slowest complete
	// answer you expect. Zero means no limit.
	HeaderTimeout time.Duration
	// Jitter is the fraction (0–1) of a computed backoff that is
	// randomized, so callers that failed together do not retry
	// together. Header-requested delays are never jittered.
	Jitter float64

	// AfterSuccess retries a response the rules above would hand back,
	// once per request, when the call's session has already been served
	// (callctx.PriorCallSucceeded) and AfterSuccess says so. It is for
	// a rejection that is ambiguous on its own: Vertex's bare 400
	// INVALID_ARGUMENT names nothing, so on a first call it is as likely
	// a malformed request as a transient fault, but on a session whose
	// previous call under the same config succeeded it has been
	// transient every time it was recorded (core-agent #898, #1247).
	// It is consulted only for a 4xx or 5xx; body is that response's
	// body, at most 64 KiB, and stays readable by the caller when the
	// response is handed back. Nil disables it.
	AfterSuccess func(status int, body []byte) bool

	// Sleep waits for d or until ctx is done. Nil means a real timer;
	// tests substitute it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock used to resolve an HTTP-date Retry-After. Nil
	// means time.Now.
	Now func() time.Time
	// Rand returns a float in [0, 1) for jitter. Nil means math/rand.
	Rand func() float64
}

// Default is the policy adapters install unless configured otherwise:
// two retries, starting at one second.
func Default() Policy {
	return Policy{
		MaxRetries:     2,
		InitialBackoff: time.Second,
		BackoffFactor:  2,
		MaxBackoff:     20 * time.Second,
		MaxHeaderDelay: time.Minute,
		// A server that accepts a request and never answers would
		// otherwise hold the caller forever: observed on a Vertex AI
		// partner model, one request waiting 28 minutes for headers.
		HeaderTimeout: 5 * time.Minute,
		Jitter:        0.25,
	}
}

// GaveUp says why a retried request stopped retrying with a failure
// still in hand.
type GaveUp string

const (
	// Budget means MaxRetries was spent.
	Budget GaveUp = "budget"
	// HeaderDelay means the server asked to wait longer than
	// MaxHeaderDelay.
	HeaderDelay GaveUp = "header_delay"
	// NotReplayable means the request body could not be sent again.
	NotReplayable GaveUp = "not_replayable"
	// Canceled means the context ended during a wait.
	Canceled GaveUp = "canceled"
)

// ErrNoResponse is the error of an attempt that received no response
// headers within Policy.HeaderTimeout.
var ErrNoResponse = errors.New("retry: no response headers within the header timeout")

// Record is what Transport did for the requests made under one call's
// context. An adapter puts one on the context with WithRecord and, when
// the call ends, stamps a non-empty Snapshot onto the response, so a
// retry is visible in the transcript and not only in a log.
//
// One model call can make several HTTP requests (a pause_turn
// continuation, for instance); a Record accumulates across them. Safe
// for concurrent use.
type Record struct {
	mu   sync.Mutex
	snap Snapshot
}

// MetadataKey is the llm.Response.CustomMetadata key an adapter stamps
// a non-empty Snapshot under.
const MetadataKey = "core_models.retry"

// Snapshot is a Record's contents at one moment, in the shape it is
// persisted. Zero means no request was retried.
type Snapshot struct {
	Retries    int    `json:"retries,omitempty"`
	WaitedMS   int64  `json:"waited_ms,omitempty"`
	LastStatus int    `json:"last_status,omitempty"`
	GaveUp     GaveUp `json:"gave_up,omitempty"`
}

// Snapshot returns the record's current contents. Nil-safe.
func (r *Record) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snap
}

func (r *Record) retried(wait time.Duration, status int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snap.Retries++
	r.snap.WaitedMS += wait.Milliseconds()
	r.snap.LastStatus = status
}

func (r *Record) gaveUp(why GaveUp, status int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snap.GaveUp = why
	if status != 0 {
		r.snap.LastStatus = status
	}
}

type recordKey struct{}

// WithRecord returns a context carrying a fresh Record, and the Record.
func WithRecord(ctx context.Context) (context.Context, *Record) {
	r := &Record{}
	return context.WithValue(ctx, recordKey{}, r), r
}

func recordFrom(ctx context.Context) *Record {
	r, _ := ctx.Value(recordKey{}).(*Record)
	return r
}

// Transport returns an http.RoundTripper that retries base's
// retryable failures under p. A nil base means http.DefaultTransport.
func (p Policy) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{p: p, base: base}
}

type transport struct {
	p    Policy
	base http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	rec := recordFrom(ctx)
	backoff := t.p.InitialBackoff
	afterSuccessUsed := false
	for attempt := 0; ; attempt++ {
		resp, err := t.attempt(req)
		if !retryable(ctx, resp, err) {
			if afterSuccessUsed || !t.afterSuccess(ctx, resp, err) {
				return resp, err
			}
			afterSuccessUsed = true
		}
		status := statusOf(resp)
		if attempt >= t.p.MaxRetries {
			if t.p.MaxRetries > 0 {
				rec.gaveUp(Budget, status)
			}
			return resp, err
		}
		wait, fromHeader := t.p.delay(resp, backoff)
		if fromHeader && wait > t.p.MaxHeaderDelay {
			rec.gaveUp(HeaderDelay, status)
			return resp, err
		}
		next, ok := rewind(req)
		if !ok {
			rec.gaveUp(NotReplayable, status)
			return resp, err
		}
		discard(resp)
		if serr := t.p.sleep(ctx, wait); serr != nil {
			rec.gaveUp(Canceled, status)
			return nil, serr
		}
		rec.retried(wait, status)
		req = next
		if !fromHeader {
			backoff = t.p.grow(backoff)
		}
	}
}

// afterSuccess reports whether Policy.AfterSuccess licenses one retry
// of resp. It reads resp's body to decide and puts it back, so a
// response that is not retried reaches the caller intact.
func (t *transport) afterSuccess(ctx context.Context, resp *http.Response, err error) bool {
	// The status first, and the body only for an error: a success is
	// never AfterSuccess's business, and reading ahead into a 200 would
	// buffer a stream before the caller saw its first token.
	if t.p.AfterSuccess == nil || err != nil || resp == nil || resp.StatusCode < http.StatusBadRequest ||
		ctx.Err() != nil || !callctx.PriorCallSucceeded(ctx) {
		return false
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), rest), rest}
	if rerr != nil {
		return false
	}
	return t.p.AfterSuccess(resp.StatusCode, body)
}

// attempt sends req once, abandoning it if no response headers arrive
// within HeaderTimeout. The deadline covers the wait for headers only;
// once they are in, the body reads under the caller's context alone.
func (t *transport) attempt(req *http.Request) (*http.Response, error) {
	if t.p.HeaderTimeout <= 0 {
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	var timedOut atomic.Bool
	timer := time.AfterFunc(t.p.HeaderTimeout, func() {
		timedOut.Store(true)
		cancel()
	})
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if !timer.Stop() && timedOut.Load() {
		cancel()
		discard(resp)
		return nil, fmt.Errorf("%w (%s): %w", ErrNoResponse, t.p.HeaderTimeout, err)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	// The attempt's context must outlive this call for the body to be
	// readable, and must end when the body is done with.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// retryable reports whether a round trip's outcome is worth re-sending.
func retryable(ctx context.Context, resp *http.Response, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if errors.Is(err, ErrNoResponse) {
			return true
		}
		// Every RoundTrip error is a transport failure — no response
		// arrived — and is worth another attempt, as openai-go and
		// anthropic-sdk-go also judge. That includes errors net/http does
		// not export, such as "server closed idle connection" when a
		// reused keep-alive connection was dropped. Two kinds are not:
		// a canceled context, and a certificate the server will present
		// again, unchanged, on the next attempt.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return !certificateError(err)
	}
	switch strings.ToLower(resp.Header.Get("x-should-retry")) {
	case "true":
		return true
	case "false":
		return false
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// delay is how long to wait before the next attempt, and whether the
// server asked for it.
func (p Policy) delay(resp *http.Response, backoff time.Duration) (time.Duration, bool) {
	if d, ok := p.headerDelay(resp); ok {
		return d, true
	}
	if p.MaxBackoff > 0 && backoff > p.MaxBackoff {
		backoff = p.MaxBackoff
	}
	if p.Jitter > 0 {
		r := rand.Float64
		if p.Rand != nil {
			r = p.Rand
		}
		spread := float64(backoff) * p.Jitter
		backoff = time.Duration(float64(backoff) - spread + 2*spread*r())
	}
	return backoff, false
}

func (p Policy) headerDelay(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	if v := resp.Header.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := resp.Header.Get("retry-after")
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second)), true
	}
	if at, err := http.ParseTime(v); err == nil {
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		if d := at.Sub(now()); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func (p Policy) grow(d time.Duration) time.Duration {
	f := p.BackoffFactor
	if f < 1 {
		f = 1
	}
	return time.Duration(float64(d) * f)
}

func (p Policy) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rewind returns a request that can be sent again: the same request
// when it has no body, a clone with a fresh body when GetBody can make
// one, and false when the body has been consumed for good.
func rewind(req *http.Request) (*http.Request, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	next := req.Clone(req.Context())
	next.Body = body
	return next, true
}

// discard drains and closes a response that will not be returned, so
// its connection can be reused.
func discard(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func certificateError(err error) bool {
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
