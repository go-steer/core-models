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

// Originally derived from go-steer/mast@020004fe:internal/pricing/introductory_rates_test.go

package pricing_test

import (
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-models/pricing"
)

// TestIntroductoryRatesHaveNotOutlivedTheirExpiry runs every
// IntroductoryRates entry against every row it is billed from (the bare
// row and each backend-qualified one) at the real current time. It is
// time-dependent on purpose: that's the mechanism that turns a date in
// someone's head into a red build. It runs in the weekly regen's verify
// step too, so an expiry LiteLLM hasn't caught up with stops the regen
// instead of being rubber-stamped by it. Products run the same Check
// over the rows they bill from, so their builds go red too.
func TestIntroductoryRatesHaveNotOutlivedTheirExpiry(t *testing.T) {
	now := time.Now().UTC()
	rows := map[string]pricing.Rates{}
	for k, v := range pricing.Builtin() {
		rows[k] = v
	}
	for k, v := range pricing.BuiltinByBackend() {
		rows[k] = v
	}
	for _, ir := range pricing.IntroductoryRates() {
		t.Run(ir.Model, func(t *testing.T) {
			found := 0
			for key, r := range rows {
				if key != ir.Model && !strings.HasSuffix(key, "/"+ir.Model) {
					continue
				}
				found++
				if v := ir.Check(r, now); v.Fail {
					t.Errorf("%s: %s", key, v.Message)
				} else if v.Message != "" {
					t.Logf("%s: %s", key, v.Message)
				}
			}
			if found == 0 {
				t.Skipf("%s is no longer in the builtin table; drop its entry", ir.Model)
			}
		})
	}
}

// TestIntroductoryRateCheck pins Check's five outcomes with fixed dates.
func TestIntroductoryRateCheck(t *testing.T) {
	expiry := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	before, after := expiry.Add(-24*time.Hour), expiry.Add(24*time.Hour)
	intro := pricing.Rates{InputPerMTok: 0.75, OutputPerMTok: 3.75}
	moved := pricing.Rates{InputPerMTok: 1.5, OutputPerMTok: 7.5}
	ir := pricing.IntroductoryRate{Model: "m", NotBefore: expiry, IntroInputPerMTok: 0.75, IntroOutputPerMTok: 3.75, Standard: "$1.50/$7.50", Why: "w"}
	resolved := ir
	resolved.Resolved = "made permanent"

	for name, tc := range map[string]struct {
		ir      pricing.IntroductoryRate
		r       pricing.Rates
		now     time.Time
		fail    bool
		hasNote bool
	}{
		"before expiry, still intro":  {ir, intro, before, false, false},
		"before expiry, moved early":  {ir, moved, before, false, true},
		"after expiry, moved (spent)": {ir, moved, after, false, true},
		"after expiry, still intro":   {ir, intro, after, true, true},
		"resolved, still at intro":    {resolved, intro, after, false, false},
		"resolved, repriced again":    {resolved, moved, before, true, true},
	} {
		v := tc.ir.Check(tc.r, tc.now)
		if v.Fail != tc.fail || (v.Message != "") != tc.hasNote {
			t.Errorf("%s: Fail=%v message=%q; want Fail=%v, message present=%v", name, v.Fail, v.Message, tc.fail, tc.hasNote)
		}
	}
}

// IntroductoryRates hands out a copy, so a caller can't edit the list.
func TestIntroductoryRatesIsACopy(t *testing.T) {
	a := pricing.IntroductoryRates()
	a[0].Model = "changed"
	if pricing.IntroductoryRates()[0].Model == "changed" {
		t.Error("IntroductoryRates returned the package's own slice")
	}
}
