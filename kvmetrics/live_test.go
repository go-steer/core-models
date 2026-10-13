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

package kvmetrics_test

// Live sampling against a real server (never in presubmit):
//
//	CORE_MODELS_LIVE_METRICS_URL=http://host:8000/metrics \
//	CORE_MODELS_LIVE_METRICS_FORMAT=vllm \
//	CORE_MODELS_LIVE_CHAT_URL=http://host:8000/v1/chat/completions \
//	CORE_MODELS_LIVE_MODEL=<id> CORE_MODELS_LIVE_API_KEY=<key> \
//	go test -tags live -run TestLiveWindow -v ./kvmetrics/
//
// With CHAT_URL unset it samples an idle window.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-steer/core-models/kvmetrics"
	"github.com/go-steer/core-models/profile"
)

func TestLiveWindow(t *testing.T) {
	url := os.Getenv("CORE_MODELS_LIVE_METRICS_URL")
	if url == "" {
		t.Skip("CORE_MODELS_LIVE_METRICS_URL not set")
	}
	format := profile.MetricsFormat(os.Getenv("CORE_MODELS_LIVE_METRICS_FORMAT"))
	if format == "" {
		format = profile.MetricsVLLM
	}
	model := os.Getenv("CORE_MODELS_LIVE_MODEL")
	sm, err := kvmetrics.New(kvmetrics.Options{URL: url, Format: format, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := sm.Start(ctx, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if chat := os.Getenv("CORE_MODELS_LIVE_CHAT_URL"); chat != "" {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "Say hello in five words."}},
			"max_tokens": 32,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, chat, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if k := os.Getenv("CORE_MODELS_LIVE_API_KEY"); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chat request: %s", resp.Status)
		}
	}
	w, err := run.Stop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("window: fleet=%v readings=%d reset=%v queries=%v hits=%v rate=%v peakKV=%v",
		w.Fleet, w.Readings, w.Reset, val(w.Queries), val(w.Hits), val(w.HitRate), val(w.PeakKVUsage))
	if !w.Fleet || w.Readings < 2 {
		t.Errorf("window = %+v", w)
	}
}
