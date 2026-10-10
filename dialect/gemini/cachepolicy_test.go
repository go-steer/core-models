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

package gemini

import (
	"context"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/callctx"
	"github.com/go-steer/core-models/llm"
)

// cacheProbe is a model with every cache hook wired to a live cache
// named "c", for "gemini-3.6-flash", counting seeds.
func cacheProbe(fake *fakeLLM) (*model, *int) {
	seeds := 0
	return &model{
		inner: fake, builtins: searchAndURL(),
		cacheModel: "gemini-3.6-flash",
		cacheName:  func(context.Context) string { return "c" },
		cacheInit:  func(context.Context, *genai.Content, []*genai.Tool) { seeds++ },
	}, &seeds
}

func sysReq() *llm.Request {
	return &llm.Request{Config: &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}}}
}

// TestAToolConfigIsKeptAndTheTurnRunsUncached is fix 2: Vertex refuses
// tool_config beside cached_content and the cache holds none, so a
// request that forces a function call must run uncached, its tool
// config intact, rather than lose it to the strip.
func TestAToolConfigIsKeptAndTheTurnRunsUncached(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	m, seeds := cacheProbe(fake)
	req := sysReq()
	forced := &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}
	req.Config.ToolConfig = forced
	drain(t, m.GenerateContent(context.Background(), req, false))
	c := fake.last().Config
	if c.CachedContent != "" || c.ToolConfig == nil || c.ToolConfig.FunctionCallingConfig == nil || c.SystemInstruction == nil {
		t.Errorf("sent cache %q, tool config %+v, sys %v; want uncached with the forced mode kept", c.CachedContent, c.ToolConfig, c.SystemInstruction)
	}
	if *seeds != 0 {
		t.Errorf("a tool-config request seeded the cache %d times", *seeds)
	}
}

// TestTheCacheServesOnlyItsModel is fix 3: another model id, or a
// req.Model override, must never be stamped with this model's cache.
func TestTheCacheServesOnlyItsModel(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		inner, override string
		stamped         bool
	}{
		"the cache's model":              {"gemini-3.6-flash", "", true},
		"another model handle":           {"gemini-3.5-flash-lite", "", false},
		"a request overriding the model": {"gemini-3.6-flash", "gemini-3.5-flash-lite", false},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLLM{name: tc.inner}
			m, seeds := cacheProbe(fake)
			req := sysReq()
			req.Model = tc.override
			drain(t, m.GenerateContent(context.Background(), req, false))
			if got := fake.last().Config.CachedContent != ""; got != tc.stamped {
				t.Errorf("stamped = %v, want %v", got, tc.stamped)
			}
			if !tc.stamped && *seeds != 0 {
				t.Errorf("seeded %d times from another model's request", *seeds)
			}
		})
	}
}

// TestSideCallsAndPromptCacheOptOutsLeaveTheCacheAlone is fix 4: a
// side call's system instruction and tools are not the agent's, so it
// is neither stamped (which would swap in the agent's) nor seeded.
func TestSideCallsAndPromptCacheOptOutsLeaveTheCacheAlone(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]context.Context{
		"side call":         callctx.AsSideCall(context.Background(), "approver"),
		"prompt cache off":  callctx.WithoutPromptCache(context.Background()),
		"built-ins opt-out": callctx.WithoutBuiltins(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLLM{}
			m, seeds := cacheProbe(fake)
			m.cacheName = func(context.Context) string { return "" } // a seed would fire here
			drain(t, m.GenerateContent(ctx, sysReq(), false))
			m.cacheName = func(context.Context) string { return "c" }
			drain(t, m.GenerateContent(ctx, sysReq(), false))
			if c := fake.last().Config; c.CachedContent != "" || c.SystemInstruction == nil {
				t.Errorf("stamped %q, sys %v; want the request's own, uncached", c.CachedContent, c.SystemInstruction)
			}
			if *seeds != 0 {
				t.Errorf("seeded %d times", *seeds)
			}
		})
	}
}

// TestTheEvictionRetryIsAFullUncachedTurn is fix 5: the re-send after a
// gone cache carries the built-ins a normal uncached turn would.
func TestTheEvictionRetryIsAFullUncachedTurn(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{scripts: [][]fakeEvent{{{err: expiredErr}}, {{resp: text("ok")}}}}
	m, _ := cacheProbe(fake)
	drain(t, m.GenerateContent(context.Background(), sysReq(), false))
	retry := fake.last().Config
	if retry.CachedContent != "" || countBuiltins(retry.Tools) != 2 || retry.SystemInstruction == nil {
		t.Errorf("retry sent cache %q, %d built-ins, sys %v; want an uncached turn with both built-ins", retry.CachedContent, countBuiltins(retry.Tools), retry.SystemInstruction)
	}
}

// TestAnEmptyRetryDoesNotRestampAnEvictedCache is fix 6: once a cache
// is found gone, the empty-answer retry in the same call asks the
// manager again instead of re-sending the dead name, and the eviction
// names the cache it is about.
func TestAnEmptyRetryDoesNotRestampAnEvictedCache(t *testing.T) {
	t.Parallel()
	silent := []fakeEvent{{resp: &llm.Response{FinishReason: genai.FinishReasonStop}}}
	// Attempt 1: cached call fails gone, its uncached re-send is empty.
	// Attempt 2 (the empty retry): must run uncached.
	fake := &fakeLLM{scripts: [][]fakeEvent{{{err: reapedErr}}, silent, {{resp: text("ok")}}}}
	live := "c"
	var evicted []string
	m, _ := cacheProbe(fake)
	m.cacheName = func(context.Context) string { return live }
	m.cacheInvalidate = func(name, _ string) {
		evicted = append(evicted, name)
		if name == live {
			live = ""
		}
	}
	if _, err := drain(t, m.GenerateContent(context.Background(), sysReq(), false)); err != nil {
		t.Fatal(err)
	}
	if fake.calls() != 3 {
		t.Fatalf("calls = %d, want 3 (cached, uncached re-send, empty retry)", fake.calls())
	}
	if c := fake.last().Config; c.CachedContent != "" {
		t.Errorf("the empty retry re-stamped %q", c.CachedContent)
	}
	if len(evicted) != 1 || evicted[0] != "c" {
		t.Errorf("evictions = %q, want one, naming the stamped cache", evicted)
	}
}
