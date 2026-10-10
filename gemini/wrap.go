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
	"github.com/go-steer/core-models/gemini/vertexcache"
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
// cache reference because the cache is gone (vertexcache.Gone), so the
// next call runs uncached and a later one re-creates it. The reason
// quotes the error.
type ContextCacheInvalidateFn func(reason string)

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
	// A copy, because the stamping and appending below must not reach
	// the caller's request — a retry or a reused request would carry
	// them twice.
	r := llm.Request{}
	if req != nil {
		r = *req
	}
	cfg := genai.GenerateContentConfig{}
	if r.Config != nil {
		cfg = *r.Config
	}
	r.Config = &cfg

	// One-shot callers (core-agent's side question) opt out of both
	// built-ins and the cache for this request. Read once, so a request
	// cannot come out half-suppressed.
	suppressed := callctx.BuiltinsSuppressed(ctx)

	// The cache reference goes first. Vertex rejects a request that
	// sets CachedContent together with tools, a system instruction or a
	// tool config, so a cached turn strips them — the cache holds them
	// — and skips the built-ins, which were captured into the cache.
	// The stripped fields are kept so the eviction retry can restore
	// them; without that the uncached retry would reach the model with
	// no system prompt and no tools.
	cached := false
	var saved struct {
		system     *genai.Content
		tools      []*genai.Tool
		toolConfig *genai.ToolConfig
	}
	if !suppressed && m.cacheName != nil {
		if name := m.cacheName(ctx); name != "" {
			saved.system, saved.tools, saved.toolConfig = cfg.SystemInstruction, cfg.Tools, cfg.ToolConfig
			cfg.CachedContent = name
			cfg.SystemInstruction, cfg.Tools, cfg.ToolConfig = nil, nil, nil
			cached = true
		}
	}
	if !suppressed && !cached && len(m.builtins) > 0 && m.builtinsCompatible(&r) {
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
	}
	// Seed the cache after the built-ins are in, so cached and uncached
	// turns offer the same tools. A suppressed request carries no
	// system instruction or tools, and seeding from it would build a
	// cache that later turns run against with neither.
	if !suppressed && !cached && m.cacheInit != nil {
		m.cacheInit(ctx, cfg.SystemInstruction, cfg.Tools)
	}

	// Composed innermost first: the base call; the eviction retry (a
	// no-op on uncached turns); the empty-tail detector; the
	// retry-once on an empty answer. The eviction retry sits inside the
	// empty retry so a turn can be rescued from both, which are
	// unrelated — one is server state, the other a silent STOP. A
	// transient HTTP failure never reaches here: package retry handles
	// it below the base model.
	return retryOnceOnEmpty(m.notef, func() iter.Seq2[*llm.Response, error] {
		return emptyTail(m.evictionRetry(ctx, &r, stream, cached, saved.system, saved.tools, saved.toolConfig))
	})
}

// evictionRetry re-sends a cached turn once, uncached, when Vertex
// rejects the cache reference because the cache is gone. It tells the
// manager first, so later turns stop stamping the dead name and a
// fresh cache gets created; restores the fields the cached turn
// stripped; and drops whatever the failed attempt yielded, which the
// retry supersedes. One retry, and it is uncached, so it cannot fail
// the same way twice.
func (m *model) evictionRetry(ctx context.Context, req *llm.Request, stream, cached bool,
	system *genai.Content, tools []*genai.Tool, toolConfig *genai.ToolConfig,
) iter.Seq2[*llm.Response, error] {
	first := m.inner.GenerateContent(ctx, req, stream)
	if !cached {
		return first
	}
	return func(yield func(*llm.Response, error) bool) {
		type item struct {
			resp *llm.Response
			err  error
		}
		var buf []item
		flushed := false
		for resp, err := range first {
			if flushed {
				if !yield(resp, err) {
					return
				}
				continue
			}
			if vertexcache.Gone(err) {
				if m.cacheInvalidate != nil {
					// Quote the error rather than name a status: this
					// fires on an expiry 400 as readily as a 404
					// (core-agent #902).
					m.cacheInvalidate("GenerateContent rejected the cached content reference: " + err.Error())
				}
				m.notef("cached content unusable server-side (%v), retrying uncached", err)
				cfg := *req.Config
				cfg.CachedContent = ""
				cfg.SystemInstruction, cfg.Tools, cfg.ToolConfig = system, tools, toolConfig
				retry := *req
				retry.Config = &cfg
				for r2, e2 := range m.inner.GenerateContent(ctx, &retry, stream) {
					if !yield(r2, e2) {
						return
					}
				}
				return
			}
			buf = append(buf, item{resp, err})
			if err != nil || (resp != nil && resp.Content != nil && len(resp.Content.Parts) > 0) {
				for _, b := range buf {
					if !yield(b.resp, b.err) {
						return
					}
				}
				buf, flushed = nil, true
			}
		}
		if !flushed {
			for _, b := range buf {
				if !yield(b.resp, b.err) {
					return
				}
			}
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

// retryOnceOnEmpty runs fn, and runs it once more if the whole
// iteration ended in ErrEmptyResponse without anything usable. Chunks
// are held until the first usable one, then passed through; an empty
// attempt's chunks are discarded, so the caller never records the
// empty turn. Two attempts at most.
func retryOnceOnEmpty(notef func(string, ...any), fn func() iter.Seq2[*llm.Response, error]) iter.Seq2[*llm.Response, error] {
	const attempts = 2
	return func(yield func(*llm.Response, error) bool) {
		type item struct {
			resp *llm.Response
			err  error
		}
		for attempt := 1; attempt <= attempts; attempt++ {
			var buf []item
			flushed, empty := false, false
			for resp, err := range fn() {
				if flushed {
					if !yield(resp, err) {
						return
					}
					continue
				}
				if errors.Is(err, ErrEmptyResponse) {
					empty = true
					continue
				}
				if err == nil && usable(resp) {
					for _, b := range buf {
						if !yield(b.resp, b.err) {
							return
						}
					}
					buf, flushed = nil, true
					if !yield(resp, err) {
						return
					}
					continue
				}
				buf = append(buf, item{resp, err})
			}
			if flushed {
				if attempt > 1 {
					notef("empty response recovered on retry (attempt %d/%d)", attempt, attempts)
				}
				return
			}
			if empty && attempt < attempts {
				notef("empty response detected — retrying (attempt %d/%d)", attempt+1, attempts)
				continue
			}
			for _, b := range buf {
				if !yield(b.resp, b.err) {
					return
				}
			}
			if empty {
				notef("empty response persisted after retry")
				yield(nil, ErrEmptyResponse)
			}
			return
		}
	}
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
