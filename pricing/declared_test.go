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

// Originally derived from go-steer/core-agent@57a1500f:pkg/pricing/declared_test.go

package pricing

import (
	"testing"

	"github.com/go-steer/core-models/profile"
)

// Declared rates price a model nothing else covers, and lose to every
// other layer for one something does: a profile's declaration is the
// operator's guess, a catalog row is a published price.
func TestDeclaredIsTheLowestLayer(t *testing.T) {
	var builtinID string
	for id := range builtin {
		builtinID = id
		break
	}
	c, err := NewCatalog(Options{Declared: map[string]ModelRates{
		"google/gemma-4-26B-A4B-it": {InputPerMTok: 0.1, OutputPerMTok: 0.4},
		builtinID:                   {InputPerMTok: 999, OutputPerMTok: 999},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r, src, ok := c.LookupWithSource("google/gemma-4-26b-a4b-it")
	if !ok || src != SourceDeclared || r.InputPerMTok != 0.1 || r.OutputPerMTok != 0.4 {
		t.Errorf("declared-only model = %+v %q %v, want the declared rates from %q", r, src, ok, SourceDeclared)
	}
	if r, src, _ := c.LookupWithSource(builtinID); src != SourceBuiltin || r.InputPerMTok == 999 {
		t.Errorf("%s = %+v from %q, want the builtin row to outrank a declaration", builtinID, r, src)
	}
	if n := c.Counts().Declared; n != 2 {
		t.Errorf("Counts().Declared = %d, want 2", n)
	}
}

// TestDeclaredFromProfilesIsScopedToItsBackend: a profile's declared
// price reaches LookupFor under its own backend, does not price the same
// id on another backend, and still loses to a catalog row for a model a
// catalog prices (core-models L6).
func TestDeclaredFromProfilesIsScopedToItsBackend(t *testing.T) {
	house := profile.Profile{Name: "house-vllm", Models: []profile.Model{
		{ID: "google/gemma-4-26B-A4B-it", Rates: &profile.Rates{InputPerMTok: 0.1, OutputPerMTok: 0.4, CacheWritePerMTok: 0.125}},
		{ID: "unpriced-model"},
	}}
	lab := profile.Profile{Name: "lab", Backend: "lab-sglang", Models: []profile.Model{
		{ID: "google/gemma-4-26B-A4B-it", Rates: &profile.Rates{InputPerMTok: 0.2, OutputPerMTok: 0.8}},
	}}
	declared := DeclaredFromProfiles([]profile.Profile{house, lab})
	if len(declared) != 2 {
		t.Fatalf("declared = %v, want two rows (one per backend)", declared)
	}
	c, err := NewCatalog(Options{Declared: declared})
	if err != nil {
		t.Fatal(err)
	}
	r, src, ok := c.LookupForWithSource("house-vllm", "google/gemma-4-26B-A4B-it")
	if !ok || src != SourceDeclared || r.InputPerMTok != 0.1 || r.CacheCreationInputPerMTok != 0.125 {
		t.Errorf("house-vllm gemma = %+v %q %v, want its declared rates", r, src, ok)
	}
	if r, _, _ := c.LookupForWithSource("lab-sglang", "google/gemma-4-26B-A4B-it"); r.InputPerMTok != 0.2 {
		t.Errorf("lab-sglang gemma input = %v, want its own 0.2", r.InputPerMTok)
	}
	if _, _, ok := c.LookupForWithSource("other", "google/gemma-4-26B-A4B-it"); ok {
		t.Error("a declaration priced the same id on another backend")
	}
	if _, ok := c.LookupFor("house-vllm", "unpriced-model"); ok {
		t.Error("a model with no declared rate was priced; self-hosted is unpriced by default")
	}
	if DeclaredFromProfiles([]profile.Profile{{Name: "none"}}) != nil {
		t.Error("no declarations should give a nil map")
	}
}
