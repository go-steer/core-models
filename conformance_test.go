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

package coremodels_test

// The conformance corpus: real exchanges recorded from real servers by
// the live smoke (CORE_MODELS_LIVE_RECORD), replayed offline in
// presubmit. Per recorded exchange the replay server checks that the
// adapter sent the same request it sent then, and answers with what the
// server answered then; the scenario's own assertions run on the
// result. A change to request encoding, response parsing, think-tag
// splitting or usage mapping that a real server would notice shows up
// here without one.
//
// To add a server: run the live smoke against it with
// CORE_MODELS_LIVE_RECORD=testdata/conformance/<server>, write a
// meta.json naming the profile and model, and redact anything
// identifying from the request paths.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	coremodels "github.com/go-steer/core-models"
	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/profile"
)

type corpusMeta struct {
	Profile string `json:"profile"`
	Model   string `json:"model"`
	// ExtraBody is the model's extra_body when the recording was made, so
	// the replay sends the same request.
	ExtraBody map[string]any `json:"extra_body,omitempty"`
}

func TestConformance(t *testing.T) {
	dirs, err := filepath.Glob("testdata/conformance/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no conformance corpus found (%v)", err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta corpusMeta
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			for _, sc := range scenarios {
				t.Run(sc.name, func(t *testing.T) {
					rec := loadExchanges(t, filepath.Join(dir, sc.name+".jsonl"))
					rp := &replay{t: t, exchanges: rec}
					srv := httptest.NewServer(rp)
					defer srv.Close()

					p := profile.Profile{Name: "replay", Extends: meta.Profile, BaseURL: srv.URL + "/v1"}
					if meta.ExtraBody != nil {
						p.Models = []profile.Model{{ID: meta.Model, ExtraBody: meta.ExtraBody}}
					}
					prov, err := coremodels.Open(context.Background(), p, coremodels.Options{Resolve: profile.Options{
						Getenv: func(k string) string { return map[string]string{"GOOGLE_CLOUD_PROJECT": "PROJECT"}[k] },
						Auth: auth.Options{DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) {
							return staticToken("replay"), nil
						}},
					}})
					if err != nil {
						t.Fatal(err)
					}
					m, err := prov.Model(context.Background(), meta.Model)
					if err != nil {
						t.Fatal(err)
					}
					sc.run(t, m)
					if rp.served() != len(rec) {
						t.Errorf("replayed %d of %d recorded exchanges: the scenario no longer makes the calls it made when recorded", rp.served(), len(rec))
					}
				})
			}
		})
	}
}

func loadExchanges(t *testing.T, path string) []exchange {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []exchange
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e exchange
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, e)
	}
	return out
}

type replay struct {
	t         *testing.T
	mu        sync.Mutex
	exchanges []exchange
	n         int
}

func (r *replay) served() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func (r *replay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	n := r.n
	r.n++
	r.mu.Unlock()
	if n >= len(r.exchanges) {
		r.t.Errorf("request %d: the recording has only %d exchanges", n+1, len(r.exchanges))
		http.Error(w, "recording exhausted", http.StatusTeapot)
		return
	}
	e := r.exchanges[n]
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		r.t.Errorf("request %d went to %s", n+1, req.URL.Path)
	}
	body, _ := io.ReadAll(req.Body)
	var got, want any
	_ = json.Unmarshal(body, &got)
	_ = json.Unmarshal(e.RequestBody, &want)
	if !reflect.DeepEqual(got, want) {
		r.t.Errorf("request %d differs from the recording:\n got %s\nwant %s", n+1, body, e.RequestBody)
	}
	w.Header().Set("Content-Type", e.ContentType)
	w.WriteHeader(e.Status)
	_, _ = io.WriteString(w, e.ResponseBody)
}
