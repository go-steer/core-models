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

package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// deprecatedChat is an eligible chat entry with the given deprecation date.
func deprecatedChat(date string) liteLLMEntry {
	f := func(v float64) *float64 { return &v }
	window, yes := 1048576, true
	return liteLLMEntry{
		Mode: "chat", SupportsFunctionCalling: &yes, MaxInputTokens: &window,
		InputCostPerToken: f(3e-7), OutputCostPerToken: f(2.5e-6),
		DeprecationDate: date,
	}
}

func day(t time.Time) string { return t.Format("2006-01-02") }

func TestEligible_DeprecationGrace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		date   string
		keep   bool
		reason string
	}{
		{"deprecated today", day(today), true, ""},
		{"deprecated yesterday", day(today.AddDate(0, 0, -1)), true, ""},
		{"last day of grace", day(today.Add(-deprecationGrace).AddDate(0, 0, 1)), true, ""},
		{"grace ends today", day(today.Add(-deprecationGrace)), false, "grace ended"},
		{"deprecated 91 days ago", day(today.AddDate(0, 0, -91)), false, "grace ended"},
		{"deprecated next year", day(today.AddDate(1, 0, 0)), true, ""},
		{"unparseable date", "soon", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ok, _, why := eligible("gemini-9.5-flash", deprecatedChat(tc.date), today)
			if ok != tc.keep || !strings.Contains(why, tc.reason) {
				t.Errorf("eligible = %v (%q), want %v (%q)", ok, why, tc.keep, tc.reason)
			}
			okP, whyP := eligibleProfile(deprecatedChat(tc.date), today)
			if okP != tc.keep || !strings.Contains(whyP, tc.reason) {
				t.Errorf("eligibleProfile = %v (%q), want %v (%q)", okP, whyP, tc.keep, tc.reason)
			}
		})
	}
}

// TestGenerate_DeprecationGraceKeepsEveryRow is the 2026-10-20 case: a
// date on the bare key alone, the prefixed copies undated. Every row the
// model had stays, the window included.
func TestGenerate_DeprecationGraceKeepsEveryRow(t *testing.T) {
	t.Parallel()
	yesterday := day(today.AddDate(0, 0, -1))
	all := map[string]liteLLMEntry{
		"gemini-9.5-flash":           deprecatedChat(yesterday),
		"gemini/gemini-9.5-flash":    deprecatedChat(""),
		"vertex_ai/gemini-9.5-flash": deprecatedChat(""),
	}
	kept, qualified, _ := generate(all, today)
	if len(kept) != 1 || kept[0].Name != "gemini-9.5-flash" || kept[0].ContextWindowTokens != 1048576 {
		t.Fatalf("kept = %+v, want the bare row with its window", kept)
	}
	var names []string
	for _, e := range qualified {
		names = append(names, e.Name)
	}
	if want := []string{"gemini/gemini-9.5-flash", "vertex/gemini-9.5-flash"}; !slices.Equal(names, want) {
		t.Errorf("qualified = %v, want %v", names, want)
	}
	grace := inGrace(all, append(kept, qualified...), today)
	if len(grace) != 1 || grace[0].Name != "gemini-9.5-flash" || !strings.Contains(grace[0].Why, "kept until") {
		t.Errorf("inGrace = %+v, want gemini-9.5-flash reported as kept until a date", grace)
	}

	// Past the grace, all three go: the refused bare key keeps the
	// undated prefixed copies out, as before.
	all["gemini-9.5-flash"] = deprecatedChat(day(today.AddDate(0, 0, -91)))
	kept, qualified, _ = generate(all, today)
	if len(kept) != 0 || len(qualified) != 0 {
		t.Errorf("past grace: kept %v, qualified %v, want neither", kept, qualified)
	}
}

func TestDiscoverPrefixOnly_DeprecationGrace(t *testing.T) {
	t.Parallel()
	all := map[string]liteLLMEntry{
		"vertex_ai/gemini-9-old":  deprecatedChat(day(today.AddDate(0, 0, -1))),
		"vertex_ai/gemini-9-gone": deprecatedChat(day(today.AddDate(0, 0, -91))),
	}
	bare, _, rejected := discoverPrefixOnly(all, today)
	if len(bare) != 1 || bare[0].Name != "gemini-9-old" {
		t.Errorf("bare = %+v, want gemini-9-old (in grace) only", bare)
	}
	if len(rejected) != 1 || rejected[0].Name != "vertex_ai/gemini-9-gone" || !strings.Contains(rejected[0].Why, "grace ended") {
		t.Errorf("rejected = %+v, want vertex_ai/gemini-9-gone past grace", rejected)
	}
}

// TestEligible_RetiredUpstreamBeatsGrace: a model a probe showed gone is
// dropped by name, inside its grace too.
func TestEligible_RetiredUpstreamBeatsGrace(t *testing.T) {
	t.Parallel()
	ok, _, why := eligible("gemini-3-pro-preview", deprecatedChat(day(today.AddDate(0, 0, -1))), today)
	if ok || !strings.Contains(why, "retired upstream") {
		t.Errorf("eligible = %v (%q), want refused as retired upstream", ok, why)
	}
}
