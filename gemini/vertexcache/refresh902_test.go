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

// Originally derived from go-steer/core-agent@9d3eba89:internal/vertexcache/manager_test.go (#902)

package vertexcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"
)

// TestManager_RefreshNotFoundInvalidatesInsteadOfRidingItOut is the
// #902 regression, and it is the earlier of the two recovery points:
// Caches.Update answering "not found" is Vertex telling us the cache
// has already been reaped, so the manager can drop the handle before
// a single turn fails.
//
// It used to log and return, on the reasoning that the cache stayed
// valid until its TTL and that GenerateContent would notice the
// eviction. Neither held — the cache was already gone, and the
// generate path reports it in a shape the caller's detector missed —
// so the manager went on handing out a dead name for the life of the
// process. Live blast radius: 27h-old daemon, every cached turn in
// every session failing with a config_error.
//
// The assertion that matters is Name() == "": that is what makes the
// next turn run uncached instead of failing, and what lets Init
// create a replacement.
func TestManager_RefreshNotFoundInvalidatesInsteadOfRidingItOut(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{
		ttlOverride: 500 * time.Millisecond, // born inside the refresh window
		updateErr:   errors.New("Error 404, Message: Cached content 7016131366404227072 is not found., Status: NOT_FOUND, Details: []"),
	}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute,
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	if got := m.Name(context.Background()); got == "" {
		t.Fatal("Name returned empty before the refresh — test cannot observe the transition")
	}
	waitFor(t, testWait, func() bool { return fake.updateCount.Load() >= 1 })

	waitFor(t, testWait, func() bool { return m.Name(context.Background()) == "" })
	if snap := m.Snapshot(); snap.Active {
		t.Error("manager still Active after Update reported the cache gone; Name will keep stamping a dead handle")
	}

	// Not sticky the way stateFailed is: eviction is a normal
	// end-of-TTL event and the next Init must be allowed to create a
	// replacement, or the daemon runs uncached forever instead.
	fake.mu.Lock()
	fake.updateErr = nil
	fake.nextCacheNameOnce = "projects/p/locations/l/cachedContents/second"
	fake.mu.Unlock()
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool {
		return m.Name(context.Background()) == "projects/p/locations/l/cachedContents/second"
	})
}

// TestManager_RefreshTransientErrorKeepsTheCache is the other side of
// the branch above: a refresh that fails for any reason OTHER than
// the cache being gone must leave the handle alone. The cache is
// still live until its TTL elapses, the next Name() inside the window
// retries, and throwing away a working cache on a blip would cost a
// full re-create for nothing.
func TestManager_RefreshTransientErrorKeepsTheCache(t *testing.T) {
	t.Parallel()
	fake := &fakeCaches{
		ttlOverride: 500 * time.Millisecond,
		updateErr:   errors.New("Error 503, Message: The service is currently unavailable., Status: UNAVAILABLE"),
	}
	m := NewManager(fake, "gemini-2.5-flash", Options{
		TTL:              time.Hour,
		RefreshThreshold: 30 * time.Minute,
	})
	m.Init(context.Background(), &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Active })

	name := m.Name(context.Background())
	waitFor(t, testWait, func() bool { return fake.updateCount.Load() >= 1 })

	if got := m.Name(context.Background()); got != name {
		t.Errorf("Name = %q after a transient refresh failure, want the unchanged %q", got, name)
	}
	if !m.Snapshot().Active {
		t.Error("a 503 on refresh must not invalidate a cache that is still live")
	}
}
