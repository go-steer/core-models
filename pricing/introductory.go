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

package pricing

import (
	"fmt"
	"time"
)

// IntroductoryRate is a promotional rate with a known end date, which
// the builtin table cannot express: LiteLLM's catalog carries no expiry
// on any row, so a promotional number keeps being re-read, freshly
// stamped, after it has become false. Rates.UpdatedAt measures freshness,
// not truth. This list is where the date lives, and Check turns it into
// a test failure.
//
// Both directions happen. A rate can double on schedule (gemini-3.x-flash
// on 2027-01-01) or the increase can be cancelled and the promotional
// number made standard (claude-sonnet-5, 2026-09-01), so an entry either
// expires or is Resolved.
type IntroductoryRate struct {
	Model string
	// NotBefore is the first instant at which still carrying the
	// introductory rate is a defect: the day after the published expiry,
	// since "expires the 31st" doesn't say which hour.
	NotBefore time.Time
	// IntroInputPerMTok and IntroOutputPerMTok are the promotional rates.
	// The check fires only while a row carries exactly these numbers.
	IntroInputPerMTok, IntroOutputPerMTok float64
	// Standard is what to expect after the expiry, for the message.
	Standard string
	// Why it matters, for the message.
	Why string
	// Resolved, when set, records that the increase won't happen: the
	// check inverts and pins the row AT the former promotional rate. The
	// text says what was checked, and where.
	Resolved string
}

// IntroductoryRates returns the known promotional rates. Products run
// Check over each, against the rows they actually bill from (their
// backend-qualified rows too), so their own builds go red when a rate
// they depend on lapses, whatever core-models version they pin.
func IntroductoryRates() []IntroductoryRate {
	out := make([]IntroductoryRate, len(introductoryRates))
	copy(out, introductoryRates)
	return out
}

// Verdict is the outcome of IntroductoryRate.Check.
type Verdict struct {
	// Fail is true when the row is wrong: past expiry and still on the
	// promotional rate, or a Resolved entry whose row has moved.
	Fail bool
	// Message explains the outcome, failing or not; empty when there is
	// nothing to say.
	Message string
}

// Check judges one row against the entry at time now. Its outcomes:
//   - Resolved, row still at the former promotional rate: fine.
//   - Resolved, row moved: Fail (the provider repriced again, or the
//     table is wrong).
//   - Before NotBefore: fine; a note if the row moved early.
//   - After NotBefore, row moved: fine; a note that the entry is spent.
//   - After NotBefore, row unchanged: Fail.
func (ir IntroductoryRate) Check(r Rates, now time.Time) Verdict {
	still := r.InputPerMTok == ir.IntroInputPerMTok && r.OutputPerMTok == ir.IntroOutputPerMTok
	switch {
	case ir.Resolved != "":
		if still {
			return Verdict{}
		}
		return Verdict{Fail: true, Message: fmt.Sprintf(
			"%s prices at $%g in / $%g out, but its entry records $%g / $%g as permanent.\n"+
				"What was checked: %s.\n"+
				"Either the provider repriced again (confirm on the public pricing page and update "+
				"or drop the entry) or the table is wrong.",
			ir.Model, r.InputPerMTok, r.OutputPerMTok, ir.IntroInputPerMTok, ir.IntroOutputPerMTok, ir.Resolved)}
	case now.Before(ir.NotBefore):
		if still {
			return Verdict{}
		}
		return Verdict{Message: fmt.Sprintf("%s moved off its introductory rate early (now $%g in / $%g out); its entry can be dropped",
			ir.Model, r.InputPerMTok, r.OutputPerMTok)}
	case !still:
		return Verdict{Message: fmt.Sprintf("%s is past %s and off its introductory rate ($%g in / $%g out); its entry can be dropped",
			ir.Model, ir.NotBefore.Format("2006-01-02"), r.InputPerMTok, r.OutputPerMTok)}
	default:
		return Verdict{Fail: true, Message: fmt.Sprintf(
			"%s still prices at its introductory rate ($%g in / $%g out) on %s, past the %s expiry; expected %s.\n"+
				"UpdatedAt says %s, which is why nothing else caught this: the row is freshly read from LiteLLM and still wrong.\n"+
				"Why it matters: %s.\n"+
				"To resolve: confirm the published rate, then wait for LiteLLM (the next weekly regen) or override the row, "+
				"and update the entry (Resolved if the increase was cancelled).",
			ir.Model, r.InputPerMTok, r.OutputPerMTok, now.Format("2006-01-02"), ir.NotBefore.Format("2006-01-02"),
			ir.Standard, r.UpdatedAt.Format("2006-01-02"), ir.Why)}
	}
}

var introductoryRates = []IntroductoryRate{
	{
		Model:              "claude-sonnet-5",
		NotBefore:          time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		IntroInputPerMTok:  2,
		IntroOutputPerMTok: 10,
		Standard:           "$3 in / $15 out per MTok",
		Why: "claude-sonnet-5 is a product mid-tier default on Anthropic, so tier:mid work prices through it, " +
			"including cost ceilings: an understated rate lets a workload spend past its ceiling",
		Resolved: "checked 2026-08-20 against platform.claude.com/docs/en/about-claude/pricing: " +
			"\"The $2/$10 per million input/output token pricing for Claude Sonnet 5, announced at launch as " +
			"introductory pricing through August 31, 2026, is now the standard price. The previously scheduled " +
			"increase to $3/$15 per million input/output tokens on September 1, 2026 will not occur.\"",
	},
	{
		Model:              "gemini-3.7-flash",
		NotBefore:          time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC),
		IntroInputPerMTok:  0.75,
		IntroOutputPerMTok: 3.75,
		Standard:           "$1.50 in / $7.50 out per MTok (and CachedInputPerMTok 0.075 -> 0.15)",
		Why: "gemini-3.7-flash was a frontier default until 2026-10-06 and configurations pinned then still price " +
			"through it; the move is a doubling, so a cost ceiling sized against $0.75/$3.75 buys half the tokens " +
			"it was budgeted for",
	},
	{
		Model:              "gemini-3.8-flash",
		NotBefore:          time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC),
		IntroInputPerMTok:  0.75,
		IntroOutputPerMTok: 3.75,
		Standard:           "$1.50 in / $7.50 out per MTok (and CachedInputPerMTok 0.075 -> 0.15)",
		Why: "arrived in the table at the promotional number, so it never looked promotional; it is the gemini/vertex " +
			"frontier default since 2026-10-06. Verified 2026-09-08 against ai.google.dev/gemini-api/docs/pricing: " +
			"input \"$0.75 through December 31, 2026\" / \"$1.50 starting January 1, 2027\", output $3.75 -> $7.50, " +
			"cached $0.075 -> $0.15",
	},
	{
		Model:              "gemini-3.6-flash",
		NotBefore:          time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC),
		IntroInputPerMTok:  0.75,
		IntroOutputPerMTok: 3.75,
		Standard:           "$1.50 in / $7.50 out per MTok (and CachedInputPerMTok 0.075 -> 0.15)",
		Why: "Google moved this older model onto the newer one's introductory rate (a 2026-08-19 regen halved it); " +
			"the January standard price is what it cost at launch, so pinned configurations read the same doubling",
	},
}
