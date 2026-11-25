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

//go:build live

package coremodels_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"google.golang.org/genai"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/profile"
)

// TestLiveRequestShapes sends the request shapes whose acceptance varies
// by model (thinking off, a thinking budget, sampling knobs, every
// thinking level) through the adapter to a real server, and fails on any
// error. It is the live half of the measured tables in dialect/anthropic
// (rejectsSampling, applyThinkingOff, claudeEffort) and dialect/gemini
// (fitThinking): a model that breaks a row shows up here. For Gemini on
// Vertex AI use CORE_MODELS_LIVE_PROFILE=vertex with GOOGLE_CLOUD_PROJECT
// and GOOGLE_CLOUD_LOCATION=global.
//
//	CORE_MODELS_LIVE_PROFILE=anthropic-vertex \
//	ANTHROPIC_VERTEX_PROJECT_ID=<project> CLOUD_ML_REGION=global \
//	CORE_MODELS_LIVE_MODELS=claude-sonnet-5-5,claude-opus-5-5 \
//	  go test -tags live -run TestLiveRequestShapes -v .
//
// CORE_MODELS_LIVE_MODEL names a single model instead.
func TestLiveRequestShapes(t *testing.T) {
	name := os.Getenv("CORE_MODELS_LIVE_PROFILE")
	list := os.Getenv("CORE_MODELS_LIVE_MODELS")
	if list == "" {
		list = os.Getenv("CORE_MODELS_LIVE_MODEL")
	}
	if name == "" || list == "" {
		t.Skip("set CORE_MODELS_LIVE_PROFILE and CORE_MODELS_LIVE_MODELS (or CORE_MODELS_LIVE_MODEL)")
	}
	p, err := profile.Find(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prov, err := coremodels.Open(ctx, p, coremodels.Options{})
	if err != nil {
		t.Fatal(err)
	}
	budget := func(n int32) *genai.ThinkingConfig { return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr(n)} }
	sampling := func(c *genai.GenerateContentConfig) *genai.GenerateContentConfig {
		c.Temperature, c.TopP, c.TopK = genai.Ptr[float32](0.2), genai.Ptr[float32](0.9), genai.Ptr[float32](40)
		return c
	}
	shapes := []struct {
		name string
		cfg  *genai.GenerateContentConfig
	}{
		{"default", &genai.GenerateContentConfig{}},
		{"thinking off", &genai.GenerateContentConfig{ThinkingConfig: budget(0)}},
		{"thinking budget", &genai.GenerateContentConfig{ThinkingConfig: budget(2048)}},
		{"sampling knobs", sampling(&genai.GenerateContentConfig{})},
		{"sampling knobs, thinking off", sampling(&genai.GenerateContentConfig{ThinkingConfig: budget(0)})},
		{"sampling knobs, thinking budget", sampling(&genai.GenerateContentConfig{ThinkingConfig: budget(2048)})},
	}
	for _, l := range []genai.ThinkingLevel{
		genai.ThinkingLevelMinimal, genai.ThinkingLevelLow, genai.ThinkingLevelMedium,
		genai.ThinkingLevelHigh, genai.ThinkingLevelXhigh, llm.ThinkingLevelMax,
	} {
		shapes = append(shapes, struct {
			name string
			cfg  *genai.GenerateContentConfig
		}{"level " + string(l), &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: l}}})
	}
	shapes = append(shapes, struct {
		name string
		cfg  *genai.GenerateContentConfig
	}{"level with budget", &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh, ThinkingBudget: genai.Ptr[int32](2048)}}})
	for _, id := range strings.Split(list, ",") {
		id = strings.TrimSpace(id)
		m, err := prov.Model(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, sh := range shapes {
			t.Run(id+"/"+sh.name, func(t *testing.T) {
				cfg := *sh.cfg
				cfg.MaxOutputTokens = 4096
				req := &llm.Request{
					Contents: []*genai.Content{genai.NewContentFromText("Reply with the single word: ok", genai.RoleUser)},
					Config:   &cfg,
				}
				for _, err := range m.GenerateContent(ctx, req, false) {
					if err != nil {
						t.Errorf("%v", err)
					}
				}
			})
		}
	}
}
