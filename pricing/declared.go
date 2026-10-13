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

import "github.com/go-steer/core-models/profile"

// DeclaredFromProfiles collects the rates provider profiles declare
// (models[].rates) into Options.Declared, keyed "<backend>/<model>" with
// the profile's backend name (profile.Profile.BackendName). LookupFor
// reaches them through that qualified key, so two profiles that declare
// different prices for the same model id each get their own, and a
// declaration never prices the same id on another backend. Pass expanded
// profiles (profile.Expand), so a profile that inherits its rates
// through extends declares them too.
//
// A later profile overrides an earlier one with the same backend and
// model.
func DeclaredFromProfiles(ps []profile.Profile) map[string]ModelRates {
	out := map[string]ModelRates{}
	for _, p := range ps {
		backend := p.BackendName()
		for _, m := range p.Models {
			r, ok := p.RatesFor(m.ID)
			if !ok {
				continue
			}
			out[backend+"/"+m.ID] = ModelRates{
				InputPerMTok:              r.InputPerMTok,
				CachedInputPerMTok:        r.CachedInputPerMTok,
				CacheCreationInputPerMTok: r.CacheWritePerMTok,
				OutputPerMTok:             r.OutputPerMTok,
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
