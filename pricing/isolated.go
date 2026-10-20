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

package pricing

import (
	"fmt"
	"maps"
	"strings"

	"github.com/go-steer/core-models/profile"
)

// applyProfiles seeds c.isolated from the built-in profiles, then
// applies ps: each is expanded, its declared rates join c.declared
// below Options.Declared's, and its pricing mode sets its backend's.
func (c *Catalog) applyProfiles(ps []profile.Profile) error {
	for _, name := range profile.BuiltinNames() {
		if p, ok := profile.Builtin(name); ok && p.IsolatedPricing() {
			c.setIsolated(p.BackendName(), true)
		}
	}
	if len(ps) == 0 {
		return nil
	}
	expanded := make([]profile.Profile, 0, len(ps))
	modes := map[string]string{} // backend -> the profile that set its mode
	for _, p := range ps {
		x, err := profile.Expand(p)
		if err != nil {
			return fmt.Errorf("pricing: %w", err)
		}
		expanded = append(expanded, x)
		backend := strings.ToLower(x.BackendName())
		isolated := x.IsolatedPricing()
		if prev, ok := modes[backend]; ok && c.isolated[backend] != isolated {
			return fmt.Errorf("pricing: profiles %q and %q share backend %q but disagree on pricing (isolated or shared); give one its own backend",
				prev, x.Name, backend)
		}
		modes[backend] = x.Name
		c.setIsolated(backend, isolated)
	}
	// Options.Declared outranks a profile's declaration for the same key:
	// it is what the product built deliberately.
	declared := lowerKeys(DeclaredFromProfiles(expanded))
	if declared == nil {
		declared = map[string]Rates{}
	}
	maps.Copy(declared, c.declared)
	if len(declared) > 0 {
		c.declared = declared
	}
	return nil
}

func (c *Catalog) setIsolated(backend string, on bool) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	switch {
	case backend == "":
	case on:
		if c.isolated == nil {
			c.isolated = map[string]bool{}
		}
		c.isolated[backend] = true
	default:
		delete(c.isolated, backend)
	}
}

// isIsolated reports whether backend is priced only from its own rows.
func (c *Catalog) isIsolated(backend string) bool { return c.isolated[backend] }
