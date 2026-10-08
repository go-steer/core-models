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

package callctx_test

import (
	"context"
	"testing"

	"github.com/go-steer/core-models/callctx"
)

func TestMarkersAreAbsentUntilApplied(t *testing.T) {
	ctx := context.Background()
	if callctx.SideCallName(ctx) != "" || callctx.BuiltinsSuppressed(ctx) || callctx.PromptCacheSuppressed(ctx) || callctx.PriorSuccessFrom(ctx) != nil {
		t.Fatal("a bare context carries a marker")
	}
	//nolint:staticcheck // SA1012: a nil context must read as "no marker", not panic.
	if callctx.SideCallName(nil) != "" || callctx.BuiltinsSuppressed(nil) || callctx.PromptCacheSuppressed(nil) || callctx.PriorSuccessFrom(nil) != nil {
		t.Fatal("a nil context carries a marker")
	}
	ctx = callctx.WithoutPromptCache(callctx.WithoutBuiltins(callctx.AsSideCall(ctx, "approver")))
	if callctx.SideCallName(ctx) != "approver" || !callctx.BuiltinsSuppressed(ctx) || !callctx.PromptCacheSuppressed(ctx) {
		t.Fatal("an applied marker is missing")
	}
}

func TestPriorSuccess(t *testing.T) {
	var nilRec *callctx.PriorSuccess
	nilRec.Mark() // nil-safe
	if nilRec.Succeeded() {
		t.Fatal("a nil record succeeded")
	}

	rec := callctx.NewPriorSuccess()
	ctx := callctx.WithPriorSuccess(context.Background(), rec)
	if callctx.PriorCallSucceeded(ctx) {
		t.Fatal("succeeded before Mark")
	}
	rec.Mark()
	if !callctx.PriorCallSucceeded(ctx) {
		t.Fatal("not succeeded after Mark")
	}
	if callctx.PriorCallSucceeded(callctx.AsSideCall(ctx, "title")) {
		t.Error("a side call leaned on its session's success")
	}
	if callctx.PriorCallSucceeded(callctx.WithPriorSuccess(ctx, nil)) {
		t.Error("a nil record did not shadow the parent's")
	}
}
