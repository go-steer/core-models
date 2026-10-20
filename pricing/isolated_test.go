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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-models/profile"
)

// unisolated is c with isolation switched off: the reference the tests
// compare against, so they hold LookupForWithSource to its own
// resolution order rather than a copy of it.
func unisolated(c *Catalog) *Catalog {
	u := *c
	u.isolated = nil
	return &u
}

// A gateway that bills in its own units must not inherit the first-party
// list price of every model it also offers: through sap-ai-core,
// gemini-2.5-pro and gpt-5-mini are unpriced until the operator says
// what SAP charges (#84). The same ids on their own backends still are.
func TestIsolatedBackendSkipsBareRows(t *testing.T) {
	c, err := NewCatalog(Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Without isolation the bare Vertex row would have priced it.
	if _, _, ok := unisolated(c).LookupForWithSource("sap-ai-core", "gemini-2.5-pro"); !ok {
		t.Fatal("gemini-2.5-pro has no bare row; pick a model that does, or this test proves nothing")
	}
	for _, id := range []string{"gemini-2.5-pro", "gpt-5-mini", "claude-sonnet-4-5", "gemini-2.5-pro-preview-99", "anthropic--claude-4.5-sonnet"} {
		if r, src, ok := c.LookupForWithSource("SAP-AI-Core", id); ok {
			t.Errorf("sap-ai-core %s = %+v from %q, want unpriced", id, r, src)
		}
	}
	if _, ok := c.LookupFor("vertex", "gemini-2.5-pro"); !ok {
		t.Error("vertex gemini-2.5-pro unpriced; isolation leaked into a shared backend")
	}
	if _, ok := c.LookupFor("openai", "gpt-5-mini"); !ok {
		t.Error("openai gpt-5-mini unpriced; isolation leaked into a shared backend")
	}
	if _, ok := c.Lookup("gemini-2.5-pro"); !ok {
		t.Error("bare Lookup gemini-2.5-pro unpriced")
	}
}

// Every layer prices an isolated backend through its qualified key, and
// none through a bare one: an operator's bare gpt-5-mini override is a
// statement about OpenAI's price, not SAP's.
func TestIsolatedBackendTakesQualifiedRowsFromEveryLayer(t *testing.T) {
	agents, home := t.TempDir(), t.TempDir()
	write := func(dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "pricing.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(agents, `{"version":1,"models":{"sap-ai-core/model-project":{"input_per_mtok":2,"output_per_mtok":2},"model-project":{"input_per_mtok":99,"output_per_mtok":99}}}`)
	write(home, `{"version":1,"manual":{"models":{"sap-ai-core/model-user":{"input_per_mtok":3,"output_per_mtok":3},"model-user":{"input_per_mtok":99,"output_per_mtok":99}}}}`)
	c, err := NewCatalog(Options{
		CfgOverride: map[string]ModelRates{
			"sap-ai-core/model-cfg": {InputPerMTok: 1, OutputPerMTok: 1},
			"model-cfg":             {InputPerMTok: 99, OutputPerMTok: 99},
			"gemini-2.5-pro":        {InputPerMTok: 99, OutputPerMTok: 99},
			"sap-ai-core/family":    {InputPerMTok: 5, OutputPerMTok: 5},
		},
		AgentsDir: agents,
		UserHome:  home,
		// Unexpanded: NewCatalog expands it, so the declaration lands
		// under the backend it inherits.
		Profiles: []profile.Profile{{Name: "my-sap", Extends: "sap-ai-core", Models: []profile.Model{
			{ID: "model-declared", Rates: &profile.Rates{InputPerMTok: 4, OutputPerMTok: 4}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]struct {
		in  float64
		src string
	}{
		"model-cfg":      {1, SourceCfgOverride},
		"model-project":  {2, SourceProjectFile},
		"model-user":     {3, SourceUserManual},
		"model-declared": {4, SourceDeclared},
		"family-2026":    {5, SourceCfgOverride}, // a qualified prefix still applies
	} {
		r, src, ok := c.LookupForWithSource("sap-ai-core", id)
		if !ok || r.InputPerMTok != want.in || src != want.src {
			t.Errorf("sap-ai-core %s = %+v from %q (%v), want input %v from %q", id, r, src, ok, want.in, want.src)
		}
	}
	if r, src, ok := c.LookupForWithSource("sap-ai-core", "gemini-2.5-pro"); ok {
		t.Errorf("sap-ai-core gemini-2.5-pro = %+v from %q, want an operator's bare override ignored", r, src)
	}
	if r, _, _ := c.LookupForWithSource("vertex", "gemini-2.5-pro"); r.InputPerMTok != 99 {
		t.Errorf("vertex gemini-2.5-pro input = %v, want the bare override (99) on a shared backend", r.InputPerMTok)
	}
}

// Options.Profiles sets each backend's mode: an operator's isolated
// gateway, a derived SAP profile under its own backend (isolation
// inherited through extends), and pricing "shared" on a profile that
// keeps sap-ai-core's backend, which undoes the built-in's isolation.
func TestProfilesSetPricingMode(t *testing.T) {
	c, err := NewCatalog(Options{Profiles: []profile.Profile{
		{Name: "acme-gateway", Pricing: profile.PricingIsolated},
		{Name: "sap-eu", Extends: "sap-ai-core", Backend: "sap-eu"},
		{Name: "my-sap", Extends: "sap-ai-core", Pricing: profile.PricingShared},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"acme-gateway", "Sap-EU"} {
		if r, src, ok := c.LookupForWithSource(b, "gemini-2.5-pro"); ok {
			t.Errorf("%s gemini-2.5-pro = %+v from %q, want unpriced", b, r, src)
		}
	}
	if _, ok := c.LookupFor("sap-ai-core", "gemini-2.5-pro"); !ok {
		t.Error("sap-ai-core gemini-2.5-pro unpriced, want the bare row once a profile on that backend sets pricing shared")
	}

	_, err = NewCatalog(Options{Profiles: []profile.Profile{
		{Name: "sap-a", Extends: "sap-ai-core"},
		{Name: "sap-b", Extends: "sap-ai-core", Pricing: profile.PricingShared},
	}})
	if err == nil || !strings.Contains(err.Error(), `"sap-a" and "sap-b" share backend "sap-ai-core"`) {
		t.Errorf("disagreeing profiles on one backend: err = %v, want them named", err)
	}
	if _, err := NewCatalog(Options{Profiles: []profile.Profile{{Name: "x", Extends: "no-such"}}}); err == nil {
		t.Error("a profile extending an unknown built-in was accepted")
	}
}

// Options.Declared, which the product built itself, outranks a passed
// profile's declaration for the same key; a profile's declaration
// joins it otherwise.
func TestProfilesDeclaredRatesJoinOptionsDeclared(t *testing.T) {
	house := profile.Profile{Name: "house", Models: []profile.Model{
		{ID: "m1", Rates: &profile.Rates{InputPerMTok: 1}},
		{ID: "m2", Rates: &profile.Rates{InputPerMTok: 2}},
	}}
	c, err := NewCatalog(Options{
		Declared: map[string]ModelRates{"house/m1": {InputPerMTok: 10}},
		Profiles: []profile.Profile{house},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := c.LookupFor("house", "m1"); r.InputPerMTok != 10 {
		t.Errorf("house m1 input = %v, want Options.Declared's 10", r.InputPerMTok)
	}
	if r, _ := c.LookupFor("house", "m2"); r.InputPerMTok != 2 {
		t.Errorf("house m2 input = %v, want the profile's 2", r.InputPerMTok)
	}
}

// Isolation changes nothing for any other backend: across every builtin
// row and every backend the table qualifies, LookupForWithSource answers
// exactly as it does with isolation switched off.
func TestIsolationLeavesSharedBackendsAlone(t *testing.T) {
	c, err := NewCatalog(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref := unisolated(c)
	backends := map[string]bool{"": true, "some-profile": true}
	ids := map[string]bool{}
	for k := range builtinByBackend {
		b, id, _ := strings.Cut(k, "/")
		backends[b] = true
		ids[id] = true
	}
	for id := range builtin {
		ids[id] = true
		ids[id+"-2099-01-01"] = true
	}
	n := 0
	for b := range backends {
		if c.isIsolated(b) {
			t.Fatalf("builtin backend %q is isolated", b)
		}
		for id := range ids {
			r, src, ok := c.LookupForWithSource(b, id)
			wr, wsrc, wok := ref.LookupForWithSource(b, id)
			if r != wr || src != wsrc || ok != wok {
				t.Fatalf("LookupFor(%q, %q) = %+v %q %v, want %+v %q %v", b, id, r, src, ok, wr, wsrc, wok)
			}
			n++
		}
	}
	if n < 1000 {
		t.Fatalf("compared only %d pairs", n)
	}
}
