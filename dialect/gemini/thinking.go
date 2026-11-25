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
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"
)

// fitThinking returns a thinking config modelID accepts: tc itself when
// nothing needs to change, otherwise a copy, since tc belongs to the
// caller.
//
// Gemini 4 takes a thinking level and nothing else. Measured 2026-11-25
// on Vertex AI `global`:
//
//	model              budget (any, 0 too)   MINIMAL   LOW-HIGH   XHIGH   budget+level
//	gemini-4-neon      400 "use level"       400       accepted   accepted   400
//	gemini-4-hydrogen  400 "use level"       400       accepted   accepted   400
//	gemini-4-carbon    400 "use level"       400       accepted   400        400
//
// So on Gemini 4: a level outranks a budget, which is dropped; a budget
// alone becomes the nearest level (budgetLevel); MINIMAL becomes LOW,
// since thinking cannot be turned off; XHIGH and llm.ThinkingLevelMax
// become the model's highest level. Argon is documented with Carbon (top
// level HIGH) and an unknown Gemini 4 or later model is given HIGH, the
// highest level every measured one accepts. Levels are read
// case-insensitively.
//
// Other Gemini models pass through unchanged, except
// llm.ThinkingLevelMax, which only core-models defines: it becomes HIGH.
func fitThinking(modelID string, tc *genai.ThinkingConfig) *genai.ThinkingConfig {
	if tc == nil {
		return nil
	}
	top, gemini4 := gemini4TopLevel(modelID)
	if !gemini4 {
		if !strings.EqualFold(string(tc.ThinkingLevel), string(llm.ThinkingLevelMax)) {
			return tc
		}
		c := *tc
		c.ThinkingLevel = genai.ThinkingLevelHigh
		return &c
	}
	c := *tc
	c.ThinkingLevel = genai.ThinkingLevel(strings.ToUpper(string(c.ThinkingLevel)))
	if (c.ThinkingLevel == "" || c.ThinkingLevel == genai.ThinkingLevelUnspecified) && c.ThinkingBudget != nil {
		c.ThinkingLevel = budgetLevel(*c.ThinkingBudget)
	}
	c.ThinkingBudget = nil
	switch c.ThinkingLevel {
	case genai.ThinkingLevelMinimal:
		c.ThinkingLevel = genai.ThinkingLevelLow
	case genai.ThinkingLevelXhigh, llm.ThinkingLevelMax:
		c.ThinkingLevel = top
	}
	return &c
}

// budgetLevel reads a token budget as the nearest Gemini 4 level. A
// negative budget is genai's "dynamic", the model's own default, so no
// level. Zero, a request not to think, is LOW: Gemini 4 cannot turn
// thinking off. The cut points are approximate, not measured
// equivalences.
func budgetLevel(budget int32) genai.ThinkingLevel {
	switch {
	case budget < 0:
		return ""
	case budget <= 4096:
		return genai.ThinkingLevelLow
	case budget <= 16384:
		return genai.ThinkingLevelMedium
	default:
		return genai.ThinkingLevelHigh
	}
}

// gemini4TopLevel reports whether modelID is a Gemini 4 or later model
// (geminiMajorVersion, so a resource path prefix and case are ignored)
// and, if so, the highest thinking level it accepts. Only the Gemini 4
// families are measured; a later model is treated as Gemini 4 with an
// unknown family, which takes no budget and tops out at HIGH.
func gemini4TopLevel(modelID string) (genai.ThinkingLevel, bool) {
	if geminiMajorVersion(modelID) < 4 {
		return "", false
	}
	if i := strings.LastIndexByte(modelID, '/'); i >= 0 {
		modelID = modelID[i+1:]
	}
	rest, ok := strings.CutPrefix(strings.ToLower(modelID), "gemini-4-")
	if !ok {
		return genai.ThinkingLevelHigh, true
	}
	family, _, _ := strings.Cut(rest, "-")
	switch family {
	case "neon", "hydrogen":
		return genai.ThinkingLevelXhigh, true
	default:
		return genai.ThinkingLevelHigh, true
	}
}
