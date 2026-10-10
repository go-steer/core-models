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

package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-steer/core-models/auth"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		c       auth.Config
		wantErr string
	}{
		"api key":         {auth.Config{Kind: auth.APIKey, Env: "OPENAI_API_KEY"}, ""},
		"bearer":          {auth.Config{Kind: auth.Bearer, Env: "TOKEN"}, ""},
		"adc":             {auth.Config{Kind: auth.GoogleADC}, ""},
		"adc with scopes": {auth.Config{Kind: auth.GoogleADC, Scopes: []string{"s"}}, ""},
		"none":            {auth.Config{Kind: auth.None}, ""},
		"missing kind":    {auth.Config{}, "kind is required"},
		"unknown kind":    {auth.Config{Kind: "oauth"}, `unknown auth kind "oauth"`},
		"api key no env":  {auth.Config{Kind: auth.APIKey}, "needs env"},
		"api key scopes":  {auth.Config{Kind: auth.APIKey, Env: "K", Scopes: []string{"s"}}, "takes no scopes"},
		"adc with env":    {auth.Config{Kind: auth.GoogleADC, Env: "X"}, "takes no env"},
		"none with env":   {auth.Config{Kind: auth.None, Env: "X"}, "takes no env"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveAKeyNamesTheVariableNotTheSecret(t *testing.T) {
	c := auth.Config{Kind: auth.APIKey, Env: "XAI_API_KEY"}
	if _, err := c.Resolve(context.Background(), auth.Options{Getenv: env(nil)}); err == nil || !strings.Contains(err.Error(), "XAI_API_KEY is not set") {
		t.Fatalf("Resolve with the variable unset = %v, want it named", err)
	}
	cred, err := c.Resolve(context.Background(), auth.Options{Getenv: env(map[string]string{"XAI_API_KEY": "sk-secret"})})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Secret() != "sk-secret" {
		t.Errorf("Secret = %q", cred.Secret())
	}
	if s := cred.String(); strings.Contains(s, "sk-secret") || !strings.Contains(s, "XAI_API_KEY") {
		t.Errorf("String = %q: must name the variable and never the secret", s)
	}
}

type fakeTokens struct {
	n   int
	err error
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.n++
	if f.err != nil {
		return "", f.err
	}
	return "ya29.token-" + string(rune('0'+f.n)), nil
}

func TestResolveGoogleADC(t *testing.T) {
	var gotScopes []string
	ts := &fakeTokens{}
	opts := auth.Options{DetectGoogle: func(_ context.Context, scopes []string) (auth.TokenSource, error) {
		gotScopes = scopes
		return ts, nil
	}}
	cred, err := auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotScopes) != 1 || gotScopes[0] != auth.CloudPlatformScope {
		t.Errorf("scopes = %v, want the cloud-platform default", gotScopes)
	}
	if cred.Secret() != "" {
		t.Errorf("Secret = %q, want empty for ADC", cred.Secret())
	}

	_, err = auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), auth.Options{
		DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) {
			return nil, errors.New("could not find default credentials")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no Google Application Default Credentials") {
		t.Errorf("Resolve with no ADC = %v, want a construction-time error", err)
	}
}

func TestBearerTransportRefreshesPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	ts := &fakeTokens{}
	cred, err := auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), auth.Options{
		DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) { return ts, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: cred.BearerTransport(nil)}
	for range 2 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if req.Header.Get("Authorization") != "" {
			t.Error("BearerTransport modified the caller's request")
		}
	}
	if len(seen) != 2 || seen[0] != "Bearer ya29.token-1" || seen[1] != "Bearer ya29.token-2" {
		t.Errorf("Authorization headers = %v, want a fresh token per request", seen)
	}
}

func TestBearerTransportForAStaticKeyAndForNone(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	key, _ := auth.Config{Kind: auth.APIKey, Env: "K"}.Resolve(context.Background(), auth.Options{Getenv: env(map[string]string{"K": "abc"})})
	none, _ := auth.Config{Kind: auth.None}.Resolve(context.Background(), auth.Options{})
	for _, tc := range []struct {
		cred *auth.Credential
		want string
	}{{key, "Bearer abc"}, {none, ""}} {
		got = "unset"
		resp, err := (&http.Client{Transport: tc.cred.BearerTransport(nil)}).Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got != tc.want {
			t.Errorf("%s: Authorization = %q, want %q", tc.cred, got, tc.want)
		}
	}
}

func TestBearerTransportSurfacesATokenFailure(t *testing.T) {
	cred, _ := auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), auth.Options{
		DetectGoogle: func(context.Context, []string) (auth.TokenSource, error) {
			return &fakeTokens{err: errors.New("refresh denied")}, nil
		},
	})
	_, err := (&http.Client{Transport: cred.BearerTransport(nil)}).Get("http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "refresh denied") {
		t.Errorf("err = %v, want the token failure", err)
	}
}

// The real detection path, against a credentials file rather than a
// metadata server: an authorized_user file parses without any network,
// and no token is fetched.
func TestResolveGoogleADCFindsACredentialsFile(t *testing.T) {
	path := t.TempDir() + "/adc.json"
	if err := os.WriteFile(path, []byte(`{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"rt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	cred, err := auth.Config{Kind: auth.GoogleADC}.Resolve(context.Background(), auth.Options{})
	if err != nil {
		t.Fatalf("Resolve = %v, want ADC found from GOOGLE_APPLICATION_CREDENTIALS", err)
	}
	if cred.Kind() != auth.GoogleADC || cred.String() != "google_adc" {
		t.Errorf("credential = %s", cred)
	}
}

// TestAltEnvIsTriedInOrder: a key that goes by two names resolves from
// either, the first set wins, and a miss names every variable tried.
func TestAltEnvIsTriedInOrder(t *testing.T) {
	c := auth.Config{Kind: auth.APIKey, Env: "GOOGLE_API_KEY", AltEnv: []string{"GEMINI_API_KEY"}}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"first":  {map[string]string{"GOOGLE_API_KEY": "a", "GEMINI_API_KEY": "b"}, "a"},
		"second": {map[string]string{"GEMINI_API_KEY": "b"}, "b"},
	} {
		cred, err := c.Resolve(context.Background(), auth.Options{Getenv: func(k string) string { return tc.env[k] }})
		if err != nil || cred.Secret() != tc.want {
			t.Errorf("%s: secret %q, err %v; want %q", name, cred.Secret(), err, tc.want)
		}
	}
	_, err := c.Resolve(context.Background(), auth.Options{Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "GOOGLE_API_KEY or GEMINI_API_KEY") {
		t.Errorf("miss = %v", err)
	}
	if err := (auth.Config{Kind: auth.GoogleADC, AltEnv: []string{"X"}}).Validate(); err == nil {
		t.Error("google_adc accepted alt_env")
	}
}
