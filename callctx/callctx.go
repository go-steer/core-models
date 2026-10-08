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

// Package callctx is the per-call vocabulary adapters and products
// share through a context.Context.
//
// One model value serves an agent's main loop and its one-shot side
// calls (an approver, a title, a summarizer) alike, so anything that
// differs per call travels on the call's context rather than on the
// model. These keys started in core-agent's pkg/models, where the
// agent, the approver and the retry policy already speak them; they
// live here so an adapter in this library and a product's code read
// the same key. core-agent's helpers become aliases of these when it
// adopts core-models (docs/design.md §11, L3').
//
// Every marker here can only take something away from a request — a
// tool, a cache marker, a retry. None of them adds behavior, so a
// marker an adapter does not honor is a missed optimization, never a
// wrong request.
package callctx

import (
	"context"
	"sync/atomic"
)

type sideCallKey struct{}

// AsSideCall marks ctx as a side call named name: a one-shot model call
// made beside the agent's main loop, with its own instruction and
// usually no tools. Retry and logging use the name to tell these calls
// from the turns a transcript shows.
func AsSideCall(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, sideCallKey{}, name)
}

// SideCallName returns the name AsSideCall gave ctx, or "".
func SideCallName(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	name, _ := ctx.Value(sideCallKey{}).(string)
	return name
}

type noBuiltinsKey struct{}

// WithoutBuiltins marks ctx as a request that must reach the model
// with exactly the tools the caller put on it: no provider-injected
// server-side built-ins (search, URL context, code execution) and no
// cache reference stamped on top. It is the per-call form; a product
// that wants no built-ins at all configures the profile instead.
func WithoutBuiltins(ctx context.Context) context.Context {
	return context.WithValue(ctx, noBuiltinsKey{}, true)
}

// BuiltinsSuppressed reports whether WithoutBuiltins was applied to
// ctx.
func BuiltinsSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(noBuiltinsKey{}).(bool)
	return v
}

type noPromptCacheKey struct{}

// WithoutPromptCache marks ctx as a request that must not place
// provider prompt-cache markers. A side call whose prefix will never be
// sent again pays the cache-write premium for nothing.
func WithoutPromptCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noPromptCacheKey{}, true)
}

// PromptCacheSuppressed reports whether WithoutPromptCache was applied
// to ctx.
func PromptCacheSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(noPromptCacheKey{}).(bool)
	return v
}

// PriorSuccess records that a model call in one session has already
// succeeded. It is evidence a retry policy may use: a rejection that is
// ambiguous on its own (Vertex's bare 400 INVALID_ARGUMENT, core-agent
// #1247) is worth one retry only when the same session, model and
// configuration have already been served, because then the request
// cannot be malformed from the start.
//
// The session owns one and marks it when a response arrives; retry
// only reads it. A nil *PriorSuccess is valid and has never succeeded.
// Safe for concurrent use.
type PriorSuccess struct {
	ok atomic.Bool
}

// NewPriorSuccess returns an unmarked record.
func NewPriorSuccess() *PriorSuccess { return &PriorSuccess{} }

// Mark records that a model call has succeeded. Nil-safe.
func (p *PriorSuccess) Mark() {
	if p != nil {
		p.ok.Store(true)
	}
}

// Succeeded reports whether Mark has been called. Nil-safe.
func (p *PriorSuccess) Succeeded() bool {
	return p != nil && p.ok.Load()
}

type priorSuccessKey struct{}

// WithPriorSuccess puts rec on ctx for the calls made under it. A nil
// rec shadows any record ctx already carries: a nested run (a subtask,
// a subagent) sends its own instruction and tools, and its parent's
// success says nothing about whether the child's request is well
// formed.
func WithPriorSuccess(ctx context.Context, rec *PriorSuccess) context.Context {
	return context.WithValue(ctx, priorSuccessKey{}, rec)
}

// PriorSuccessFrom returns the record WithPriorSuccess put on ctx, or
// nil.
func PriorSuccessFrom(ctx context.Context) *PriorSuccess {
	if ctx == nil {
		return nil
	}
	rec, _ := ctx.Value(priorSuccessKey{}).(*PriorSuccess)
	return rec
}

// PriorCallSucceeded reports whether a call under ctx may lean on its
// session's success: the session has had a call succeed, and this call
// is not a side call, whose own instruction and tools make the
// session's success no evidence about its request.
func PriorCallSucceeded(ctx context.Context) bool {
	return SideCallName(ctx) == "" && PriorSuccessFrom(ctx).Succeeded()
}
