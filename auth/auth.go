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

// Package auth resolves the credential a provider profile declares.
//
// Resolution happens when the profile is opened, not on the first
// request: a profile whose key is missing or whose Google credentials
// cannot be found fails at construction, naming what is missing
// (requirement R1). A secret never appears in an error, a log line or
// a String().
//
// How the credential is put on the wire is the dialect's business —
// OpenAI-shaped servers take a bearer token, Anthropic an x-api-key
// header, Gemini its own — so a Credential exposes the material and
// one helper for the common case, BearerTransport.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	cloudauth "cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
)

// Kind names how a profile authenticates.
type Kind string

const (
	// APIKey is a static key read from an environment variable.
	APIKey Kind = "api_key"
	// Bearer is a static bearer token read from an environment variable.
	// It differs from APIKey only in intent; both are sent the same way
	// by OpenAI-shaped dialects.
	Bearer Kind = "bearer"
	// GoogleADC is Google Application Default Credentials: the credential
	// Gemini-on-Vertex and Claude-on-Vertex already use, and the one
	// Vertex AI's partner-model endpoint takes.
	GoogleADC Kind = "google_adc"
	// None sends no credential: a self-hosted server on a trusted
	// network.
	None Kind = "none"
)

// CloudPlatformScope is the OAuth scope GoogleADC requests by default.
const CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// Config is the auth block of a profile.
type Config struct {
	Kind Kind `json:"kind" yaml:"kind"`
	// Env names the environment variable holding the key or token, for
	// APIKey and Bearer.
	Env string `json:"env,omitempty" yaml:"env,omitempty"`
	// AltEnv lists further variables tried in order when Env is unset,
	// for a vendor whose key goes by two names (Gemini's GOOGLE_API_KEY
	// and GEMINI_API_KEY).
	AltEnv []string `json:"alt_env,omitempty" yaml:"alt_env,omitempty"`
	// Scopes overrides the OAuth scopes GoogleADC requests.
	Scopes []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`
}

// Validate checks the block's shape without touching the environment.
func (c Config) Validate() error {
	switch c.Kind {
	case APIKey, Bearer:
		if c.Env == "" {
			return fmt.Errorf("auth kind %q needs env: the variable that holds the secret", c.Kind)
		}
		if len(c.Scopes) > 0 {
			return fmt.Errorf("auth kind %q takes no scopes", c.Kind)
		}
	case GoogleADC:
		if c.Env != "" || len(c.AltEnv) > 0 {
			return errors.New(`auth kind "google_adc" takes no env: credentials come from ADC`)
		}
	case None:
		if c.Env != "" || len(c.AltEnv) > 0 || len(c.Scopes) > 0 {
			return errors.New(`auth kind "none" takes no env or scopes`)
		}
	case "":
		return errors.New("auth kind is required (api_key, bearer, google_adc or none)")
	default:
		return fmt.Errorf("unknown auth kind %q (want api_key, bearer, google_adc or none)", c.Kind)
	}
	return nil
}

// TokenSource yields a current access token. *auth.Credentials from
// cloud.google.com/go/auth satisfies it through tokenSource below.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Options adjusts resolution. The zero value reads the process
// environment and real Google credentials.
type Options struct {
	// Getenv replaces os.Getenv.
	Getenv func(string) string
	// DetectGoogle replaces ADC detection.
	DetectGoogle func(ctx context.Context, scopes []string) (TokenSource, error)
}

// Credential is a resolved Config.
type Credential struct {
	kind   Kind
	env    string
	secret string
	tokens TokenSource
}

// Resolve reads the secret or finds the Google credentials c declares.
func (c Config) Resolve(ctx context.Context, opts Options) (*Credential, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	cred := &Credential{kind: c.Kind, env: c.Env}
	switch c.Kind {
	case APIKey, Bearer:
		for _, name := range append([]string{c.Env}, c.AltEnv...) {
			if v := getenv(name); v != "" {
				cred.secret, cred.env = v, name
				break
			}
		}
		if cred.secret == "" {
			return nil, fmt.Errorf("auth: %s is not set", strings.Join(append([]string{c.Env}, c.AltEnv...), " or "))
		}
	case GoogleADC:
		scopes := c.Scopes
		if len(scopes) == 0 {
			scopes = []string{CloudPlatformScope}
		}
		detect := opts.DetectGoogle
		if detect == nil {
			detect = detectGoogle
		}
		ts, err := detect(ctx, scopes)
		if err != nil {
			return nil, fmt.Errorf("auth: no Google Application Default Credentials: %w", err)
		}
		cred.tokens = ts
	}
	return cred, nil
}

func detectGoogle(_ context.Context, scopes []string) (TokenSource, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: scopes})
	if err != nil {
		return nil, err
	}
	return tokenSource{creds}, nil
}

type tokenSource struct{ creds *cloudauth.Credentials }

func (t tokenSource) Token(ctx context.Context) (string, error) {
	tok, err := t.creds.Token(ctx)
	if err != nil {
		return "", err
	}
	return tok.Value, nil
}

// Kind reports how the credential authenticates.
func (c *Credential) Kind() Kind { return c.kind }

// Secret returns the static key or token for APIKey and Bearer, and ""
// otherwise. For a dialect whose SDK takes the key directly.
func (c *Credential) Secret() string { return c.secret }

// Token returns a current token: the static secret for APIKey and
// Bearer, a fresh access token for GoogleADC, "" for None.
func (c *Credential) Token(ctx context.Context) (string, error) {
	if c.tokens != nil {
		return c.tokens.Token(ctx)
	}
	return c.secret, nil
}

// String describes the credential without its secret.
func (c *Credential) String() string {
	switch c.kind {
	case APIKey, Bearer:
		return fmt.Sprintf("%s from $%s", c.kind, c.env)
	default:
		return string(c.kind)
	}
}

// BearerTransport returns a RoundTripper that sets
// "Authorization: Bearer <token>" on every request, fetching the token
// per request so a Google access token is refreshed as it expires. For
// None it returns base unchanged. A nil base means
// http.DefaultTransport.
func (c *Credential) BearerTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if c.kind == None {
		return base
	}
	return bearer{cred: c, base: base}
}

type bearer struct {
	cred *Credential
	base http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := b.cred.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("auth: %s: %w", b.cred, err)
	}
	// A RoundTripper must not modify the request it was given.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	return b.base.RoundTrip(req)
}
