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

package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// legacyExternal is an external section as v0.13.1 and earlier wrote
// it: LiteLLM's keys verbatim, no backend-qualified rows, no Format.
// Its bare claude-sonnet-4-5 is Anthropic first-party's rate, with no
// long-context tier; Vertex's tier sits only under the vertex_ai/ key.
func legacyExternal(etag string, fetched time.Time) *UserFile {
	return &UserFile{
		Version: SchemaVersion,
		External: &ExternalSource{
			FetchedAt: fetched,
			ETag:      etag,
			Models: map[string]ModelRates{
				"claude-sonnet-4-5": {InputPerMTok: 3, OutputPerMTok: 15},
				"vertex_ai/claude-sonnet-4-5": {
					InputPerMTok: 3, OutputPerMTok: 15,
					LongContext: &LongContext{ThresholdTokens: 200_000, InputPerMTok: 6, OutputPerMTok: 22.5},
				},
			},
		},
	}
}

// sonnetBody is the LiteLLM data legacyExternal was fetched from.
const sonnetBody = `{
  "claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "litellm_provider": "anthropic", "mode": "chat"},
  "vertex_ai/claude-sonnet-4-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05,
    "input_cost_per_token_above_200k_tokens": 6e-06, "output_cost_per_token_above_200k_tokens": 2.25e-05,
    "litellm_provider": "vertex_ai-anthropic_models", "mode": "chat"}
}`

// assertSonnetVertex300K checks the case core-agent's review found:
// claude-sonnet-4-5 on anthropic-vertex at a 300K-token prompt costs
// $1.89 (Vertex's long-context tier), not the first-party $0.96.
func assertSonnetVertex300K(t *testing.T, home string) {
	t.Helper()
	cat, err := NewCatalog(Options{UserHome: home})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	const prompt, out = 300_000, 4_000
	for _, c := range []struct {
		backend string
		want    float64
	}{
		{"anthropic-vertex", 1.89},
		{"anthropic", 0.96},
	} {
		r, src, ok := cat.LookupForWithSource(c.backend, "claude-sonnet-4-5")
		if !ok {
			t.Fatalf("%s: claude-sonnet-4-5 unpriced", c.backend)
		}
		if src != SourceUserExternal {
			t.Errorf("%s: source %s, want %s", c.backend, src, SourceUserExternal)
		}
		got := r.At(prompt).CostUSD(prompt, out)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s at %d tokens: $%.4f, want $%.2f", c.backend, prompt, got, c.want)
		}
	}
}

// etagServer answers 304 to any conditional request and the full body
// otherwise, recording the If-None-Match of every request it sees.
func etagServer(t *testing.T, etag string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("If-None-Match"))
		mu.Unlock()
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(sonnetBody))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// An old-format section is upgraded on load, so the right rate applies
// before any refetch, and with no network at all.
func TestLegacyExternalQualifiedOnLoad(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := SaveUserFile(home, legacyExternal(`"old"`, time.Now())); err != nil {
		t.Fatal(err)
	}
	assertSonnetVertex300K(t, home)
}

// An old-format section is stale whatever its age: Refresh fetches in
// full, without its ETag, so a server that would answer 304 can't keep it.
func TestRefreshReplacesLegacyExternal(t *testing.T) {
	t.Parallel()
	const etag = `"same"`
	srv, seen := etagServer(t, etag)
	home := t.TempDir()
	// Ten minutes old: a current-format section would be skipped.
	if err := SaveUserFile(home, legacyExternal(etag, time.Now().Add(-10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	out, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL, Client: srv.Client()})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if out.Skipped || out.NotModified || out.NetworkFailed {
		t.Fatalf("outcome %+v, want a full fetch", out)
	}
	if got := seen(); len(got) != 1 || got[0] != "" {
		t.Errorf("If-None-Match sent %q, want one unconditional request", got)
	}
	uf, err := LoadUserFile(home)
	if err != nil {
		t.Fatal(err)
	}
	if uf.External.Format != ExternalFormat {
		t.Errorf("Format = %d, want %d", uf.External.Format, ExternalFormat)
	}
	if _, ok := uf.External.Models["anthropic-vertex/claude-sonnet-4-5"]; !ok {
		t.Error("rewritten section has no anthropic-vertex/claude-sonnet-4-5 row")
	}
	assertSonnetVertex300K(t, home)
}

// A forced refresh (MinInterval < 0) fetches in full even over a
// current-format section with an ETag.
func TestForcedRefreshSendsNoIfNoneMatch(t *testing.T) {
	t.Parallel()
	const etag = `"same"`
	srv, seen := etagServer(t, etag)
	home := t.TempDir()
	if _, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL, Client: srv.Client()}); err != nil {
		t.Fatal(err)
	}
	out, err := Refresh(context.Background(), home, RefreshOptions{
		Source: srv.URL, Client: srv.Client(), MinInterval: -1,
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if out.NotModified {
		t.Error("forced refresh was answered 304; it should not be conditional")
	}
	if got := seen(); len(got) != 2 || got[1] != "" {
		t.Errorf("If-None-Match sent %q, want the forced request unconditional", got)
	}
}

// A current-format section past its interval still revalidates with its
// ETag, and a 304 re-stamps FetchedAt without rewriting the rows.
func TestCurrentExternalRevalidates(t *testing.T) {
	t.Parallel()
	const etag = `"same"`
	srv, seen := etagServer(t, etag)
	home := t.TempDir()
	start := time.Now().Add(-48 * time.Hour)
	if _, err := Refresh(context.Background(), home, RefreshOptions{
		Source: srv.URL, Client: srv.Client(), Now: func() time.Time { return start },
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := LoadUserFile(home)
	out, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL, Client: srv.Client()})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !out.NotModified {
		t.Fatalf("outcome %+v, want NotModified", out)
	}
	if got := seen(); len(got) != 2 || got[1] != etag {
		t.Errorf("If-None-Match sent %q, want the cached ETag on revalidation", got)
	}
	after, _ := LoadUserFile(home)
	if !after.External.FetchedAt.After(start) {
		t.Error("FetchedAt not re-stamped on 304")
	}
	if after.External.Format != ExternalFormat || len(after.External.Models) != len(before.External.Models) {
		t.Errorf("304 changed the section: format %d, %d rows (was %d)", after.External.Format, len(after.External.Models), len(before.External.Models))
	}
}
