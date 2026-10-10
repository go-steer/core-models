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

// Originally derived from go-steer/mast@6568618:internal/providers/gemini/builtins_test.go and go-steer/core-agent@9d3eba89:pkg/models/gemini/builtins_test.go

package gemini

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/callctx"
	"github.com/go-steer/core-models/llm"
)

// fakeLLM records the requests it is sent and answers each call with
// the next script entry (the last one repeats).
type fakeLLM struct {
	mu      sync.Mutex
	name    string
	reqs    []*llm.Request
	scripts [][]fakeEvent
}

type fakeEvent struct {
	resp *llm.Response
	err  error
}

func (f *fakeLLM) Name() string {
	if f.name == "" {
		return "gemini-3.6-flash"
	}
	return f.name
}

func (f *fakeLLM) last() *llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeLLM) GenerateContent(_ context.Context, req *llm.Request, _ bool) iter.Seq2[*llm.Response, error] {
	f.mu.Lock()
	cp := *req
	if req.Config != nil {
		c := *req.Config
		cp.Config = &c
	}
	f.reqs = append(f.reqs, &cp)
	var events []fakeEvent
	if n := len(f.scripts); n > 0 {
		events = f.scripts[min(len(f.reqs)-1, n-1)]
	} else {
		events = []fakeEvent{{resp: text("ok")}}
	}
	f.mu.Unlock()
	return func(yield func(*llm.Response, error) bool) {
		for _, e := range events {
			if !yield(e.resp, e.err) {
				return
			}
		}
	}
}

func text(s string) *llm.Response {
	return &llm.Response{Content: genai.NewContentFromText(s, genai.RoleModel), FinishReason: genai.FinishReasonStop}
}

func drain(t *testing.T, seq iter.Seq2[*llm.Response, error]) ([]*llm.Response, error) {
	t.Helper()
	var out []*llm.Response
	var last error
	for r, err := range seq {
		if err != nil {
			last = err
			continue
		}
		out = append(out, r)
	}
	return out, last
}

func searchAndURL() []*genai.Tool {
	return BuiltinTools{GoogleSearch: true, URLContext: true}.asTools()
}

func countBuiltins(tools []*genai.Tool) int {
	n := 0
	for _, tl := range tools {
		if tl != nil && (tl.GoogleSearch != nil || tl.URLContext != nil || tl.CodeExecution != nil) {
			n++
		}
	}
	return n
}

func TestBuiltinToolsProjectInFieldOrder(t *testing.T) {
	t.Parallel()
	all := BuiltinTools{GoogleSearch: true, URLContext: true, CodeExecution: true}
	if got := all.Names(); !slices.Equal(got, []string{WebSearch, URLContext, CodeExecution}) {
		t.Errorf("Names = %v", got)
	}
	if got := (BuiltinTools{}).asTools(); got != nil {
		t.Errorf("zero BuiltinTools projected %d tools, want none (mast #324)", len(got))
	}
	b, err := BuiltinToolsFromNames([]string{URLContext, WebSearch})
	if err != nil || !b.GoogleSearch || !b.URLContext || b.CodeExecution {
		t.Errorf("FromNames = %+v, %v", b, err)
	}
	if _, err := BuiltinToolsFromNames([]string{"google_maps"}); err == nil {
		t.Error("an unknown built-in name was accepted")
	}
}

func TestBuiltinsAppendAndKeepFunctionTools(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	m := &model{inner: fake, builtins: searchAndURL()}
	fn := &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "finish_task"}}}
	tools := make([]*genai.Tool, 1, 8) // spare capacity the append must not write into
	tools[0] = fn
	req := &llm.Request{Config: &genai.GenerateContentConfig{Tools: tools}}
	drain(t, m.GenerateContent(context.Background(), req, false))

	sent := fake.last().Config.Tools
	if len(sent) != 3 || sent[0] != fn || countBuiltins(sent) != 2 {
		t.Fatalf("sent tools = %d (%d built-in); want the function tool then two built-ins", len(sent), countBuiltins(sent))
	}
	if len(req.Config.Tools) != 1 || tools[:2][1] != nil {
		t.Error("the caller's request or its tool slice was modified")
	}
}

// TestIncludeServerSideToolInvocationsFollowsTheBackend is mast #505,
// closed structurally: the flag is set from the backend, so the
// Developer API path cannot omit it and Vertex never receives it.
func TestIncludeServerSideToolInvocationsFollowsTheBackend(t *testing.T) {
	t.Parallel()
	for name, direct := range map[string]bool{"developer API": true, "vertex": false} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLLM{}
			m := &model{inner: fake, builtins: searchAndURL(), directAPI: direct}
			drain(t, m.GenerateContent(context.Background(), &llm.Request{}, false))
			tc := fake.last().Config.ToolConfig
			got := tc != nil && tc.IncludeServerSideToolInvocations != nil && *tc.IncludeServerSideToolInvocations
			if got != direct {
				t.Errorf("IncludeServerSideToolInvocations = %v, want %v", got, direct)
			}
		})
	}
}

func TestNoBuiltinsNoToolConfig(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	m := &model{inner: fake, directAPI: true}
	drain(t, m.GenerateContent(context.Background(), &llm.Request{}, false))
	if c := fake.last().Config; len(c.Tools) != 0 || c.ToolConfig != nil {
		t.Errorf("a model with no built-ins sent tools %v and tool config %+v", c.Tools, c.ToolConfig)
	}
}

// TestASuppressedRequestGetsNeitherBuiltinsNorCache is core-agent's
// per-request opt-out, now through callctx.
func TestASuppressedRequestGetsNeitherBuiltinsNorCache(t *testing.T) {
	t.Parallel()
	inits := 0
	fake := &fakeLLM{}
	m := &model{
		inner: fake, builtins: searchAndURL(),
		cacheModel: "gemini-3.6-flash", cacheName: func(context.Context) string { return "projects/p/locations/l/cachedContents/abc" },
	}
	ctx := callctx.WithoutBuiltins(context.Background())
	drain(t, m.GenerateContent(ctx, &llm.Request{}, false))
	c := fake.last().Config
	if countBuiltins(c.Tools) != 0 || c.CachedContent != "" || inits != 0 {
		t.Errorf("suppressed request: %d built-ins, cache %q, %d inits; want none", countBuiltins(c.Tools), c.CachedContent, inits)
	}
}

func TestWithoutBuiltinsDropsTheInjection(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	m := &model{inner: fake, builtins: searchAndURL(), cacheModel: "gemini-3.6-flash", cacheName: func(context.Context) string { return "c" }}
	stripped := m.WithoutBuiltins()
	drain(t, stripped.GenerateContent(context.Background(), &llm.Request{}, false))
	if c := fake.last().Config; countBuiltins(c.Tools) != 0 || c.CachedContent != "" {
		t.Errorf("WithoutBuiltins still injected: %d built-ins, cache %q", countBuiltins(c.Tools), c.CachedContent)
	}
	if got := m.BuiltinToolNames(); !slices.Equal(got, []string{WebSearch, URLContext}) {
		t.Errorf("BuiltinToolNames = %v", got)
	}
}

// TestContextCacheHooksFire is mast #221's wiring: Init sees the fully
// assembled uncached request, built-ins included; a ready cache is
// stamped; a cached turn does not re-seed.
func TestContextCacheHooksFire(t *testing.T) {
	t.Parallel()
	var inits int
	var seenSys *genai.Content
	var seenTools []*genai.Tool
	name := ""
	fake := &fakeLLM{}
	m := &model{
		inner: fake, builtins: searchAndURL(),
		cacheModel: "gemini-3.6-flash", cacheInit: func(_ context.Context, sys *genai.Content, tools []*genai.Tool) {
			inits++
			seenSys, seenTools = sys, tools
		},
		cacheName: func(context.Context) string { return name },
	}
	sys := &genai.Content{Parts: []*genai.Part{{Text: "you are a test agent"}}}
	drain(t, m.GenerateContent(context.Background(), &llm.Request{Config: &genai.GenerateContentConfig{SystemInstruction: sys}}, false))
	if inits != 1 || seenSys != sys || countBuiltins(seenTools) != 2 {
		t.Errorf("uncached turn: %d inits, sys %v, %d built-ins seeded; want 1, the request's, 2", inits, seenSys, countBuiltins(seenTools))
	}
	if fake.last().Config.CachedContent != "" {
		t.Error("CachedContent set while the name hook returned empty")
	}

	name = "projects/p/locations/l/cachedContents/abc"
	drain(t, m.GenerateContent(context.Background(), &llm.Request{Config: &genai.GenerateContentConfig{SystemInstruction: sys}}, false))
	c := fake.last().Config
	if c.CachedContent != name {
		t.Errorf("CachedContent = %q, want %q", c.CachedContent, name)
	}
	if inits != 1 {
		t.Errorf("cacheInit ran %d times; a cached turn must not re-seed", inits)
	}
}

// TestCachedTurnStripsWhatVertexForbids: Vertex 400s a request setting
// CachedContent with tools, a system instruction or a tool config.
func TestCachedTurnStripsWhatVertexForbids(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	m := &model{inner: fake, builtins: searchAndURL(), cacheModel: "gemini-3.6-flash", cacheName: func(context.Context) string { return "c" }}
	req := &llm.Request{Config: &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "sys"}}},
		Tools:             []*genai.Tool{{}, {}},
	}}
	drain(t, m.GenerateContent(context.Background(), req, false))
	c := fake.last().Config
	if c.CachedContent != "c" || c.SystemInstruction != nil || c.Tools != nil || c.ToolConfig != nil {
		t.Errorf("cached turn sent cache %q, sys %v, %d tools, tool config %v; want the cache alone", c.CachedContent, c.SystemInstruction, len(c.Tools), c.ToolConfig)
	}
	if req.Config.SystemInstruction == nil || len(req.Config.Tools) != 2 {
		t.Error("the strip reached the caller's request")
	}
}

// TestPreThreeModelsSkipTheMix pins the pre-3.0 degradation: Gemini 2.x
// rejects built-ins beside function declarations, so the wrapper skips
// them there and keeps them everywhere else.
func TestPreThreeModelsSkipTheMix(t *testing.T) {
	t.Parallel()
	fnTools := func() []*genai.Tool {
		return []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "finish_task"}}}}
	}
	for _, tc := range []struct {
		name, model string
		tools       []*genai.Tool
		want        bool
	}{
		{"2.5 with function tools skips", "gemini-2.5-pro", fnTools(), false},
		{"2.5 without function tools keeps", "gemini-2.5-pro", nil, true},
		{"3.6 with function tools keeps", "gemini-3.6-flash", fnTools(), true},
		{"3 with function tools keeps", "gemini-3-pro", fnTools(), true},
		{"unparseable with function tools skips", "custom-tuned-model", fnTools(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var notes []string
			fake := &fakeLLM{}
			m := &model{inner: fake, builtins: searchAndURL(), logf: func(f string, _ ...any) { notes = append(notes, f) }}
			req := &llm.Request{Model: tc.model, Config: &genai.GenerateContentConfig{Tools: tc.tools}}
			drain(t, m.GenerateContent(context.Background(), req, false))
			drain(t, m.GenerateContent(context.Background(), req, false))
			got := countBuiltins(fake.last().Config.Tools) > 0
			if got != tc.want {
				t.Errorf("built-ins injected = %v, want %v", got, tc.want)
			}
			if !tc.want && len(notes) != 1 {
				t.Errorf("skip notices = %d over two turns, want exactly one", len(notes))
			}
		})
	}
}

func TestGeminiMajorVersion(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]int{
		"gemini-3.6-flash": 3, "gemini-3-pro": 3, "gemini-2.5-pro": 2, "Gemini-3.6-Flash": 3,
		"gemini-10.1-pro": 10, "gemini-flash": 0, "claude-sonnet-4-6": 0, "": 0,
		"models/gemini-3.5-flash": 3, "publishers/google/models/gemini-2.5-flash": 2,
	} {
		if got := geminiMajorVersion(id); got != want {
			t.Errorf("geminiMajorVersion(%q) = %d, want %d", id, got, want)
		}
	}
}

var errBoom = errors.New("boom")
