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

package usage_test

import (
	"encoding/json"
	"testing"

	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/usage"
)

func TestAttachLeavesOtherKeysAlone(t *testing.T) {
	resp := &llm.Response{CustomMetadata: map[string]any{"source": "operator"}}
	usage.Attach(resp, &usage.Detail{CacheWriteTokens: usage.Int64(7)})
	if resp.CustomMetadata["source"] != "operator" {
		t.Errorf("Attach clobbered an unrelated key: %v", resp.CustomMetadata)
	}
	d, ok := usage.FromMetadata(resp.CustomMetadata)
	if !ok || d.CacheWriteTokens == nil || *d.CacheWriteTokens != 7 {
		t.Fatalf("FromMetadata = %+v, %v; want CacheWriteTokens 7", d, ok)
	}
}

// A replayed event has been through JSON, so the typed value is gone
// and a map stands in its place. Reported zeros must survive that and
// unreported counts must stay unreported — the distinction is the
// reason Detail exists.
func TestFromMetadataSurvivesAJSONRoundTrip(t *testing.T) {
	resp := &llm.Response{}
	usage.Attach(resp, &usage.Detail{
		CacheReadTokens: usage.Int64(0),
		ServedModel:     "gpt-oss-20b",
		Backend:         "vertex-maas",
	})
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var replayed llm.Response
	if err := json.Unmarshal(b, &replayed); err != nil {
		t.Fatal(err)
	}
	if _, isMap := replayed.CustomMetadata[usage.MetadataKey].(map[string]any); !isMap {
		t.Fatalf("after a round trip the sidecar is %T, want map[string]any — the test no longer exercises the replay path",
			replayed.CustomMetadata[usage.MetadataKey])
	}
	d, ok := usage.FromMetadata(replayed.CustomMetadata)
	if !ok {
		t.Fatal("FromMetadata found nothing after a round trip")
	}
	if d.CacheReadTokens == nil || *d.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %v, want a reported zero", d.CacheReadTokens)
	}
	if d.CacheWriteTokens != nil {
		t.Errorf("CacheWriteTokens = %v, want nil: nothing was reported", *d.CacheWriteTokens)
	}
	if d.ServedModel != "gpt-oss-20b" || d.Backend != "vertex-maas" {
		t.Errorf("identity fields = %q, %q", d.ServedModel, d.Backend)
	}
}

func TestFromMetadataWithNothingAttached(t *testing.T) {
	for name, md := range map[string]map[string]any{
		"nil map":    nil,
		"absent key": {"other": 1},
		"wrong type": {usage.MetadataKey: "not a detail"},
		"nil detail": {usage.MetadataKey: (*usage.Detail)(nil)},
	} {
		if d, ok := usage.FromMetadata(md); ok {
			t.Errorf("%s: FromMetadata = %+v, true; want false", name, d)
		}
	}
}
