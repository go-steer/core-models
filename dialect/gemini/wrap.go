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

// Originally derived from go-steer/mast@6568618:internal/providers/gemini/builtins.go, with go-steer/core-agent@9d3eba89:pkg/models/gemini/builtins.go's callctx opt-out and eviction reason

package gemini

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/callctx"
	"github.com/go-steer/core-models/dialect/gemini/vertexcache"
	"github.com/go-steer/core-models/llm"
)

// ContextCacheInitFn is called with the fully assembled system
// instruction and tools of a request about to run uncached, so the
// cache can be seeded from exactly what the model is sent. It must not
// block; vertexcache.Manager.Init creates the cache in the background.
type ContextCacheInitFn func(ctx context.Context, systemInstruction *genai.Content, tools []*genai.Tool)

// ContextCacheNameFn returns the cache to stamp on a request, or "" to
// run it uncached — always safe.
type ContextCacheNameFn func(ctx context.Context) string

// ContextCacheInvalidateFn is told that Vertex rejected the stamped
// cache reference name because the cache is gone (vertexcache.Gone), so
// later calls run uncached until a fresh cache exists. It should forget
// name only if name is still current — vertexcache.Manager's
// MarkEvictedName does exactly that. The reason quotes the error.
type ContextCacheInvalidateFn func(name, reason string)

// ErrEmptyResponse is returned when the model answered with no usable
// content, no finish reason other than STOP and no error, twice in a
// row — the silent turn mast #220 was filed for. It wraps
// llm.ErrEmptyResponse.
var ErrEmptyResponse = fmt.Errorf(
	"gemini: model returned no usable content with no finish reason and no error — likely a silent safety filter, streaming truncation, or transient Vertex fault; retrying often succeeds (%w)",
	llm.ErrEmptyResponse)

// model is the llm.LLM a Client hands out: the base model plus the
// behaviour both products layered on top of ADK's.
type model struct {
	inner     llm.LLM
	builtins  []*genai.Tool
	directAPI bool
	logf      func(format string, args ...any)

	// The cache hooks serve one model, cacheModel: a Vertex cache is
	// created for a model and serves only that one.
	cacheModel      string
	cacheInit       ContextCacheInitFn
	cacheName       ContextCacheNameFn
	cacheInvalidate ContextCacheInvalidateFn

	// mixSkipOnce limits the "built-ins skipped for this model" notice
	// to one line per model handle; the condition does not change
	// between turns.
	mixSkipOnce sync.Once
}

func (m *model) Name() string { return m.inner.Name() }

// BuiltinToolNames reports the server-side built-ins this model
// injects, under the neutral names. An upper bound, not a per-request
// promise: a pre-3.0 model carrying function tools, a request that
// opted out through callctx, and WithoutBuiltins all subtract, never
// add, so a name absent here is a tool that cannot be reached.
func (m *model) BuiltinToolNames() []string { return builtinNames(m.builtins) }

// WithoutBuiltins returns this model with no built-ins injected and no
// cache stamped, for a caller that must send exactly its own tools —
// a subtask whose tool set is the point. The empty-response safety net
// stays.
func (m *model) WithoutBuiltins() llm.LLM {
	return &model{inner: m.inner, directAPI: m.directAPI, logf: m.logf}
}

func (m *model) notef(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

func (m *model) GenerateContent(ctx context.Context, req *llm.Request, stream bool) iter.Seq2[*llm.Response, error] {
	// Copies throughout: nothing below may reach the caller's request,
	// or a retry or a reused request would carry it twice.
	base := llm.Request{}
	if req != nil {
		base = *req
	}
	cfg := genai.GenerateContentConfig{}
	if base.Config != nil {
		cfg = *base.Config
	}
	base.Config = &cfg

	// One-shot callers (core-agent's side question) opt out of both
	// built-ins and the cache. Read once, so a request cannot come out
	// half-suppressed.
	suppressed := callctx.BuiltinsSuppressed(ctx)
	uncached := m.uncached(base, suppressed)
	useCache := m.cacheable(ctx, &base, suppressed)

	// Composed innermost first: the call, cached or not; the empty-tail
	// detector; the retry-once on an empty answer. The cache decision
	// is taken inside the factory, so an attempt after an eviction asks
	// the manager again and runs uncached rather than re-stamping the
	// name it just learned is dead. A transient HTTP failure never
	// reaches here: package retry handles it below the base model.
	return retryOnceOnEmpty(m.notef, func() iter.Seq2[*llm.Response, error] {
		name := ""
		if useCache && m.cacheName != nil {
			name = m.cacheName(ctx)
		}
		if name == "" {
			// Seed from the request as it is sent uncached, built-ins
			// in, so cached and uncached turns offer the same tools.
			if useCache && m.cacheInit != nil {
				m.cacheInit(ctx, uncached.Config.SystemInstruction, uncached.Config.Tools)
			}
			return emptyTail(m.inner.GenerateContent(ctx, uncached, stream))
		}
		return emptyTail(m.cached(ctx, uncached, name, stream))
	})
}

// uncached returns req as it goes out with no cache: the built-ins
// appended where the model allows them, and the server-side flag the
// Developer API needs beside them.
func (m *model) uncached(req llm.Request, suppressed bool) *llm.Request {
	cfg := *req.Config
	req.Config = &cfg
	if suppressed || len(m.builtins) == 0 || !m.builtinsCompatible(&req) {
		return &req
	}
	cfg.Tools = append(slices.Clip(cfg.Tools), m.builtins...)
	// Gemini 3+ on the Developer API rejects built-ins mixed with
	// function calling unless this is set ("Please enable
	// tool_config.include_server_side_tool_invocations…"). Vertex AI
	// rejects the parameter itself and allows the mix anyway, so the
	// backend decides — no caller can forget it (mast #505).
	if m.directAPI {
		tc := genai.ToolConfig{}
		if cfg.ToolConfig != nil {
			tc = *cfg.ToolConfig
		}
		tc.IncludeServerSideToolInvocations = new(true)
		cfg.ToolConfig = &tc
	}
	return &req
}

// cacheable reports whether req may use the context cache, as a stamp
// or as the request it is seeded from. Not when:
//   - the request opted out of built-ins or of prompt caching, or is a
//     side call: its system instruction and tools are not the agent's,
//     and stamping would replace them with the agent's;
//   - it is for another model than the cache's;
//   - it carries a tool config. Vertex refuses tool_config beside
//     cached_content ("Tool config, tools and system instruction should
//     not be set in the request when using cached content"), and the
//     cache holds none, so stripping it would drop a forced function
//     call or a calling mode on the floor. Such a request runs uncached.
func (m *model) cacheable(ctx context.Context, req *llm.Request, suppressed bool) bool {
	if m.cacheName == nil && m.cacheInit == nil {
		return false
	}
	if suppressed || callctx.PromptCacheSuppressed(ctx) || callctx.SideCallName(ctx) != "" {
		return false
	}
	model := req.Model
	if model == "" {
		model = m.inner.Name()
	}
	return model == m.cacheModel && req.Config.ToolConfig == nil
}

// cached sends uncached's turn against the cache name: the cache holds
// the system instruction and tools, and Vertex refuses a request that
// repeats them, so they are left off. If Vertex says the cache is gone
// (vertexcache.Gone), the manager is told and the turn is re-sent once
// exactly as an uncached turn would be — built-ins and all — with
// whatever the failed attempt yielded dropped.
func (m *model) cached(ctx context.Context, uncached *llm.Request, name string, stream bool) iter.Seq2[*llm.Response, error] {
	cfg := *uncached.Config
	cfg.CachedContent = name
	cfg.SystemInstruction, cfg.Tools, cfg.ToolConfig = nil, nil, nil
	req := *uncached
	req.Config = &cfg
	return func(yield func(*llm.Response, error) bool) {
		h := hold(m.inner.GenerateContent(ctx, &req, stream), yield, vertexcache.Gone)
		switch {
		case h.stopped || h.passed:
			return
		case h.intercepted != nil:
			if m.cacheInvalidate != nil {
				// Quote the error rather than name a status: this fires
				// on an expiry 400 as readily as a 404 (core-agent #902).
				m.cacheInvalidate(name, "GenerateContent rejected the cached content reference: "+h.intercepted.Error())
			}
			m.notef("cached content unusable server-side (%v), retrying uncached", h.intercepted)
			for r, err := range m.inner.GenerateContent(ctx, uncached, stream) {
				if !yield(r, err) {
					return
				}
			}
		default:
			h.flush(yield)
		}
	}
}

// builtinsCompatible reports whether built-ins may be added to req.
// Gemini 2.5 and older reject server-side tools mixed with function
// declarations ("Multiple tools are supported only when they are all
// search tools"), and an agent loop almost always carries function
// declarations, so on an old model the injection would fail every
// turn. Skip it there — the model works, ungrounded — and say so once.
// A request with no function declarations keeps built-ins on any
// model.
func (m *model) builtinsCompatible(req *llm.Request) bool {
	if !hasFunctionDeclarations(req) {
		return true
	}
	name := req.Model
	if name == "" {
		name = m.inner.Name()
	}
	if geminiMajorVersion(name) >= 3 {
		return true
	}
	m.mixSkipOnce.Do(func() {
		m.notef("model %q predates mixed built-in + function tools (Gemini 3.0+); running without server-side built-ins for this model", name)
	})
	return false
}

func hasFunctionDeclarations(req *llm.Request) bool {
	if req == nil || req.Config == nil {
		return false
	}
	for _, t := range req.Config.Tools {
		if t != nil && len(t.FunctionDeclarations) > 0 {
			return true
		}
	}
	return false
}

// geminiMajorVersion parses the major version from a model id
// ("gemini-3.6-flash" → 3); a path-qualified id resolves by its last
// segment. 0 when it does not parse, so a version-less alias
// ("gemini-flash-latest") conservatively skips the mix.
func geminiMajorVersion(id string) int {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	rest, ok := strings.CutPrefix(strings.ToLower(id), "gemini-")
	if !ok {
		return 0
	}
	digits := rest
	if i := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = rest[:i]
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return n
}

// retryOnceOnEmpty runs fn, and runs it once more if the attempt ended
// in ErrEmptyResponse with nothing usable. An empty attempt's responses
// are dropped, so the caller never records the empty turn. Two
// attempts at most.
func retryOnceOnEmpty(notef func(string, ...any), fn func() iter.Seq2[*llm.Response, error]) iter.Seq2[*llm.Response, error] {
	const attempts = 2
	isEmpty := func(err error) bool { return errors.Is(err, ErrEmptyResponse) }
	return func(yield func(*llm.Response, error) bool) {
		for attempt := 1; attempt <= attempts; attempt++ {
			h := hold(fn(), yield, isEmpty)
			switch {
			case h.stopped:
				return
			case h.passed:
				if attempt > 1 {
					notef("empty response recovered on retry (attempt %d/%d)", attempt, attempts)
				}
				return
			case h.intercepted != nil && attempt < attempts:
				notef("empty response detected — retrying (attempt %d/%d)", attempt+1, attempts)
				continue
			}
			if !h.flush(yield) {
				return
			}
			if h.intercepted != nil {
				notef("empty response persisted after retry")
				yield(nil, ErrEmptyResponse)
			}
			return
		}
	}
}

// held is what hold saw of a sequence.
type held struct {
	passed      bool  // a usable response arrived; everything since went straight through
	stopped     bool  // the consumer stopped reading
	intercepted error // the error that ended the sequence early, if any
	items       []heldItem
}

type heldItem struct {
	resp *llm.Response
	err  error
}

// flush yields what was held, reporting false if the consumer stopped.
func (h *held) flush(yield func(*llm.Response, error) bool) bool {
	for _, it := range h.items {
		if !yield(it.resp, it.err) {
			return false
		}
	}
	return true
}

// hold is the buffer-until-usable loop both recoveries share: it holds
// seq's responses and errors until the first usable response (see
// usable), then passes that and everything after straight to yield.
// Before that point an error intercept claims ends the sequence and is
// returned, with the held items left for the caller to drop; any other
// error is held like a response. Holding is what lets a retry
// supersede an attempt's partial output instead of appending to it.
func hold(seq iter.Seq2[*llm.Response, error], yield func(*llm.Response, error) bool, intercept func(error) bool) held {
	var h held
	for resp, err := range seq {
		if h.passed {
			if !yield(resp, err) {
				h.stopped = true
				return h
			}
			continue
		}
		if err != nil && intercept(err) {
			h.intercepted = err
			return h
		}
		if err == nil && usable(resp) {
			if !h.flush(yield) || !yield(resp, nil) {
				h.stopped = true
				return h
			}
			h.items, h.passed = nil, true
			continue
		}
		h.items = append(h.items, heldItem{resp, err})
	}
	return h
}

// emptyTail passes inner through and, when it ended with nothing
// usable and no error, adds ErrEmptyResponse: the silent shapes of
// mast #220 — a turn with no parts, or a bare STOP with no content —
// that an agent loop would otherwise sit on forever.
func emptyTail(inner iter.Seq2[*llm.Response, error]) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		sawUsable, sawError := false, false
		for resp, err := range inner {
			if err != nil {
				sawError = true
			} else if usable(resp) {
				sawUsable = true
			}
			if !yield(resp, err) {
				return
			}
		}
		if !sawUsable && !sawError {
			yield(nil, ErrEmptyResponse)
		}
	}
}

// usable reports whether a response carries a signal: parts, an error,
// or a finish reason other than STOP. A bare STOP with no parts is the
// model claiming to be done having said nothing.
func usable(r *llm.Response) bool {
	switch {
	case r == nil:
		return false
	case r.Content != nil && len(r.Content.Parts) > 0:
		return true
	case r.ErrorCode != "" || r.ErrorMessage != "":
		return true
	}
	return r.FinishReason != "" && r.FinishReason != genai.FinishReasonStop
}
