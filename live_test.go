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

// Live smoke against a real server. Not part of presubmit: it needs a
// server, and for managed profiles credentials and a bill.
//
//	CORE_MODELS_LIVE_PROFILE=ollama CORE_MODELS_LIVE_MODEL=qwen3:1.7b \
//	  go test -tags live -run TestLive -v .
//
// CORE_MODELS_LIVE_RECORD=<dir> also writes every HTTP exchange, with
// the Authorization header removed, to <dir>/<scenario>.jsonl — the raw
// material of the conformance corpus.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/profile"
)

type recorder struct {
	base http.RoundTripper
	mu   sync.Mutex
	file string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := r.base.RoundTrip(req)
	if err != nil || r.file == "" {
		return resp, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	line, _ := json.Marshal(exchange{
		Method: req.Method, Path: req.URL.Path, RequestBody: reqBody,
		Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), ResponseBody: string(respBody),
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ferr := os.OpenFile(r.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	return resp, nil
}

func TestLive(t *testing.T) {
	name, modelID := os.Getenv("CORE_MODELS_LIVE_PROFILE"), os.Getenv("CORE_MODELS_LIVE_MODEL")
	if name == "" || modelID == "" {
		t.Skip("set CORE_MODELS_LIVE_PROFILE and CORE_MODELS_LIVE_MODEL")
	}
	p, err := profile.Find(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := os.Getenv("CORE_MODELS_LIVE_BASE_URL"); u != "" {
		p = profile.Profile{Name: "live", Extends: name, BaseURL: u}
	}
	rec := &recorder{base: http.DefaultTransport}
	prov, err := coremodels.Open(context.Background(), p, coremodels.Options{
		HTTPClient: &http.Client{Transport: rec, Timeout: 5 * time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := prov.Model(context.Background(), modelID)
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("CORE_MODELS_LIVE_RECORD")
	scenario := func(_ *testing.T, s string) {
		if dir == "" {
			return
		}
		_ = os.MkdirAll(dir, 0o755)
		rec.mu.Lock()
		rec.file = filepath.Join(dir, s+".jsonl")
		_ = os.Remove(rec.file)
		rec.mu.Unlock()
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			scenario(t, sc.name)
			sc.run(t, m)
		})
	}
}
