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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-models/profile"
)

// TestLookupFor_PrecedenceMatrix pins LookupForWithSource's order: exact
// keys in every layer before any prefix, layer precedence before
// qualification, qualified before bare within a layer, declared last.
func TestLookupFor_PrecedenceMatrix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		c              *Catalog
		backend, model string
		want           float64
		wantSrc        string
	}{
		{
			name: "override bare beats builtin qualified",
			c: &Catalog{
				cfgOverride: map[string]Rates{"m": {InputPerMTok: 1}},
				builtin:     map[string]Rates{"vertex/m": {InputPerMTok: 2}},
			},
			backend: "vertex", model: "m", want: 1, wantSrc: SourceCfgOverride,
		},
		{
			name: "newer external bare beats builtin qualified",
			c: &Catalog{
				userExt: map[string]Rates{"m": {InputPerMTok: 3}},
				builtin: map[string]Rates{"vertex/m": {InputPerMTok: 2}},
			},
			backend: "vertex", model: "m", want: 3, wantSrc: SourceUserExternal,
		},
		{
			name: "exact bare beats qualified prefix for a dated id",
			c: &Catalog{builtin: map[string]Rates{
				"m-20260101":   {InputPerMTok: 4},
				"anthropic/m-": {InputPerMTok: 5},
			}},
			backend: "anthropic", model: "m-20260101", want: 4, wantSrc: SourceBuiltin,
		},
		{
			name: "builtin bare beats declared qualified",
			c: &Catalog{
				builtin:  map[string]Rates{"m": {InputPerMTok: 6}},
				declared: map[string]Rates{"house/m": {InputPerMTok: 7}},
			},
			backend: "house", model: "m", want: 6, wantSrc: SourceBuiltin,
		},
		{
			name: "qualified beats bare within a layer",
			c: &Catalog{builtin: map[string]Rates{
				"m":        {InputPerMTok: 8},
				"vertex/m": {InputPerMTok: 9},
			}},
			backend: "vertex", model: "m", want: 9, wantSrc: SourceBuiltin,
		},
		{
			name: "override bare prefix beats builtin qualified prefix",
			c: &Catalog{
				cfgOverride: map[string]Rates{"m": {InputPerMTok: 10}},
				builtin:     map[string]Rates{"vertex/m": {InputPerMTok: 11}},
			},
			backend: "vertex", model: "m-20260101", want: 10, wantSrc: SourceCfgOverride,
		},
		{
			name: "qualified prefix beats longer bare prefix within a layer",
			c: &Catalog{builtin: map[string]Rates{
				"m-2026":   {InputPerMTok: 12},
				"vertex/m": {InputPerMTok: 13},
			}},
			backend: "vertex", model: "m-20260101", want: 13, wantSrc: SourceBuiltin,
		},
		{
			name:    "declared qualified prices what nothing else does",
			c:       &Catalog{declared: map[string]Rates{"house/m": {InputPerMTok: 14}}},
			backend: "house", model: "M", want: 14, wantSrc: SourceDeclared,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, src, ok := tc.c.LookupForWithSource(tc.backend, tc.model)
			if !ok || r.InputPerMTok != tc.want || src != tc.wantSrc {
				t.Errorf("LookupForWithSource(%q, %q) = %v from %q (ok=%v), want %v from %q",
					tc.backend, tc.model, r.InputPerMTok, src, ok, tc.want, tc.wantSrc)
			}
		})
	}
}

// A qualified key's prefix search stops at the backend: a key no longer
// than "<backend>/" is not a model row, and must not price every model
// the backend serves.
func TestLookupFor_QualifiedPrefixStopsAtBackend(t *testing.T) {
	t.Parallel()
	c := &Catalog{builtin: map[string]Rates{"vertex/": {InputPerMTok: 1}, "vertex": {InputPerMTok: 2}}}
	if r, ok := c.LookupFor("vertex", "m"); ok {
		t.Errorf("LookupFor(vertex, m) = %+v; a bare backend key must not price a model", r)
	}
}

// Every generated key is lowercase: the catalog folds lookups to
// lowercase, so a mixed-case key would never match.
func TestBuiltinKeysAreLowercase(t *testing.T) {
	t.Parallel()
	for name, table := range map[string]map[string]Rates{
		"builtin":          builtin,
		"builtinByBackend": builtinByBackend,
	} {
		for k := range table {
			if k != strings.ToLower(strings.TrimSpace(k)) {
				t.Errorf("%s key %q is not lowercase", name, k)
			}
		}
	}
	for k := range builtinContextWindows {
		if k != strings.ToLower(k) {
			t.Errorf("builtinContextWindows key %q is not lowercase", k)
		}
	}
}

// A pricing.json that does not parse must survive a Refresh untouched:
// replacing it would delete the operator's manual section.
func TestRefresh_UnparseableFileIsAnErrorAndUntouched(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	path := filepath.Join(home, UserFileName)
	corrupt := []byte(`{"manual": {"models": {"m": {"input_per_mtok": 1}}`) // truncated
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL, Client: srv.Client()}); err == nil {
		t.Fatal("Refresh over an unparseable pricing.json returned nil error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Errorf("pricing.json was rewritten:\n%s", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("Refresh fetched %d times before giving up; it should fail before the network", n)
	}
}

// Refreshed rows carry the refresh time, so a pricing view can say when
// each external rate was last verified.
func TestRefresh_StampsUpdatedAt(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	if _, err := Refresh(context.Background(), home, RefreshOptions{
		Source: srv.URL, Client: srv.Client(), Now: func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}
	uf, err := LoadUserFile(home)
	if err != nil || uf.External == nil || len(uf.External.Models) == 0 {
		t.Fatalf("LoadUserFile = %+v, %v; want an external section", uf, err)
	}
	for k, r := range uf.External.Models {
		if !r.UpdatedAt.Equal(now) {
			t.Errorf("%s UpdatedAt = %v, want the refresh time %v", k, r.UpdatedAt, now)
		}
	}
	c, err := NewCatalog(Options{UserHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if r, src, ok := c.LookupWithSource("gemini-3.5-flash"); !ok || src != SourceUserExternal || !r.UpdatedAt.Equal(now) {
		t.Errorf("Lookup(gemini-3.5-flash) = %+v from %q (ok=%v), want UpdatedAt %v from %q",
			r, src, ok, now, SourceUserExternal)
	}
}

// Within one profile the first rated entry for a repeated id wins, as
// profile.Profile.RatesFor reads it.
func TestDeclaredFromProfiles_DuplicateIDFirstWins(t *testing.T) {
	t.Parallel()
	p := profile.Profile{Name: "house", Models: []profile.Model{
		{ID: "m"},
		{ID: "m", Rates: &profile.Rates{InputPerMTok: 1}},
		{ID: "m", Rates: &profile.Rates{InputPerMTok: 2}},
	}}
	got := DeclaredFromProfiles([]profile.Profile{p})
	key := p.BackendName() + "/m"
	want, _ := p.RatesFor("m")
	if r, ok := got[key]; !ok || r.InputPerMTok != want.InputPerMTok {
		t.Errorf("DeclaredFromProfiles[%q] = %+v (ok=%v), want InputPerMTok %v", key, r, ok, want.InputPerMTok)
	}
}

func BenchmarkLookupFor(b *testing.B) {
	c, err := NewCatalog(Options{})
	if err != nil {
		b.Fatal(err)
	}
	for _, bc := range []struct{ name, backend, model string }{
		{"exact-qualified", "vertex", "gemini-3.5-flash"},
		{"prefix-dated", "anthropic", "claude-opus-4-7-20991231"},
		{"miss", "vertex", "totally-made-up-model-with-a-long-name"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for b.Loop() {
				c.LookupFor(bc.backend, bc.model)
			}
		})
	}
}
