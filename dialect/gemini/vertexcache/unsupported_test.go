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

package vertexcache

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// cachingUnsupported is what Vertex answered a Caches.Create for
// gemini-4-argon in the global region on 2026-10-19, as genai surfaces it.
var cachingUnsupported = genai.APIError{
	Code:    400,
	Message: "Model `gemini-4-argon` does not support explicit context caching.",
	Status:  "INVALID_ARGUMENT",
}

func TestIsCachingUnsupported(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"the real shape", cachingUnsupported, true},
		{"wrapped", fmt.Errorf("vertexcache: create: %w", cachingUnsupported), true},
		{"another model", errors.New("Error 400, Message: Model `gemini-5-pro` does not support explicit context caching., Status: INVALID_ARGUMENT, Details: []"), true},

		// --- must NOT match ---
		{"below the minimum, which has its own verdict", errors.New(belowMinimum), false},
		{"IAM propagation, which keeps its retries", errors.New("rpc error: code = PermissionDenied desc = 403"), false},
		{"bare INVALID_ARGUMENT", errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT"), false},
		{"the phrase under a non-400 status", errors.New("Error 503, Message: does not support explicit context caching right now, Status: UNAVAILABLE"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCachingUnsupported(tc.err); got != tc.want {
				t.Errorf("IsCachingUnsupported(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	if IsBelowCacheMinimum(cachingUnsupported) {
		t.Error("IsBelowCacheMinimum matched the unsupported-model error; the two verdicts must stay disjoint")
	}
}

// TestInit_CachingUnsupportedGivesUpImmediately: before the verdict, a
// model with no explicit caching ran the whole retry schedule — six
// doomed Creates over ~7.75 minutes, each logged as a failure to act on.
func TestInit_CachingUnsupportedGivesUpImmediately(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	f := &fakeCaches{createErr: cachingUnsupported}
	clk := &fakeClock{t: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	m := NewManager(f, "gemini-4-argon", Options{Logger: log.New(&logs, "", 0)})
	m.now = clk.now
	m.retryBackoff = []time.Duration{15 * time.Second, 30 * time.Second}
	sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}

	m.Init(context.Background(), sys, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Failed })

	clk.advance(time.Hour)
	m.Init(context.Background(), sys, nil)
	time.Sleep(20 * time.Millisecond)
	if got := f.createCount.Load(); got != 1 {
		t.Errorf("createCount = %d, want 1 (the model does not change between attempts)", got)
	}
	line := logs.String()
	if strings.Count(line, "\n") != 1 {
		t.Errorf("want exactly one log line, got:\n%s", line)
	}
	for _, want := range []string{"context cache: disabled", "gemini-4-argon", "does not support explicit context caching", "Nothing to fix", "implicit caching"} {
		if !strings.Contains(line, want) {
			t.Errorf("the disable line does not say %q: %s", want, line)
		}
	}
}
