package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Every default model of every known harness has a price under its runner.
func TestEveryDefaultModelHasAPrice(t *testing.T) {
	for _, h := range harnesses {
		for _, m := range []string{h.mapModel, h.reduceModel, h.applyModel} {
			if _, ok := Prices().Lookup(h.name, m); !ok {
				t.Errorf("internal/config/prices.json has no price for %s's default model %q", h.name, m)
			}
		}
	}
}

func TestParsePricesRejects(t *testing.T) {
	good := string(pricesJSON)
	for _, tc := range []struct{ name, text, want string }{
		{"not json", "{", "prices"},
		{"unknown field", strings.Replace(good, `"checked"`, `"chekced"`, 1), "chekced"},
		{"missing default", strings.Replace(good, `"opus"`, `"opus-x"`, 1), `"opus"`},
		{"zero price", strings.Replace(good, `"input_usd_per_mtok": 4,`, `"input_usd_per_mtok": 0,`, 1), "opus"},
		{"negative credits", strings.Replace(good, `"credits_per_call": 0.1`, `"credits_per_call": -1`, 1), "auto"},
		{"bad date", strings.Replace(good, `"2026-11-27"`, `"yesterday"`, 1), "checked"},
		{"bad source", strings.Replace(good, `"https://platform.claude.com`, `"platform.claude.com`, 1), "source"},
		{"unknown runner", strings.Replace(good, `"runner": "kiro"`, `"runner": "nope"`, 1), "nope"},
		{"key in another case", strings.Replace(good, `"checked"`, `"Checked"`, 1), "Checked"},
		{"model field in another case", strings.Replace(good, `"note": "Opus`, `"Note": "Opus`, 1), "Note"},
		{"duplicate key", strings.Replace(good, `"name": "kiro",`, `"name": "kiro", "name": "kiro2",`, 1), "twice"},
		{"duplicate model", strings.Replace(good, `"models": {`, `"models": {"opus": {"input_usd_per_mtok": 1, "output_usd_per_mtok": 1},`, 1), "twice"},
		{"long note", strings.Replace(good, `"note": "Opus 5.5"`, `"note": "`+strings.Repeat("x", 201)+`"`, 1), "note"},
	} {
		if _, err := ParsePrices([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	if _, err := ParsePrices(pricesJSON); err != nil {
		t.Errorf("the embedded table: %v", err)
	}
}

func TestCheckPriceUpdate(t *testing.T) {
	old := Prices()
	today := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	edit := func(pairs ...string) *PriceTable {
		t.Helper()
		text := strings.ReplaceAll(string(pricesJSON), `"checked": "2026-11-27",
      "models": {
        "haiku"`, `"checked": "2026-12-01",
      "models": {
        "haiku"`)
		for i := 0; i < len(pairs); i += 2 {
			if !strings.Contains(text, pairs[i]) {
				t.Fatalf("no %q to replace", pairs[i])
			}
			text = strings.Replace(text, pairs[i], pairs[i+1], 1)
		}
		next, err := ParsePrices([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	if err := CheckPriceUpdate(old, edit(`"input_usd_per_mtok": 2,`, `"input_usd_per_mtok": 3,`), today, false); err != nil {
		t.Errorf("a changed price: %v", err)
	}
	for _, tc := range []struct {
		name string
		next *PriceTable
		want string
	}{
		{"moved source", edit("/about-claude/pricing", "/pricing"), "source"},
		{"dropped model", edit(`"haiku"`, `"haiku-x"`), "haiku"},
		{"sourceless provider edited", edit(`"credits_per_call": 0.1`, `"credits_per_call": 999`), "kiro"},
		{"price up past 10x", edit(`"input_usd_per_mtok": 2,`, `"input_usd_per_mtok": 1e300,`), "by hand"},
		{"price down past 0.1x", edit(`"output_usd_per_mtok": 10,`, `"output_usd_per_mtok": 0.5,`), "by hand"},
		{"checked in the future", edit(`"checked": "2026-12-01"`, `"checked": "2099-01-01"`), "today"},
		{"checked in the past", edit(`"checked": "2026-12-01"`, `"checked": "1999-01-01"`), "today"},
		{"new model", edit(`"models": {`, `"models": {"zzz": {"input_usd_per_mtok": 1, "output_usd_per_mtok": 2},`), "zzz"},
	} {
		if err := CheckPriceUpdate(old, tc.next, today, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	if err := CheckPriceUpdate(old, edit(`"models": {`, `"models": {"zzz": {"input_usd_per_mtok": 1, "output_usd_per_mtok": 2},`), today, true); err != nil {
		t.Errorf("a new model, allowed: %v", err)
	}
	if err := CheckPriceUpdate(old, edit(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), false); err == nil || !strings.Contains(err.Error(), "earlier") {
		t.Errorf("a clock before the old date: err = %v", err)
	}
}

func TestObserveCost(t *testing.T) {
	sonnet, _ := Prices().Lookup(HarnessClaude, "sonnet")
	// 400k chars is 100k input tokens, at $2 per MTok, plus the output allowance at $10.
	got, unit := sonnet.Cost(1, 400_000)
	want := 100_000*2/1e6 + ObserveOutputTokens*10/1e6
	if unit != "" || math.Abs(got-want) > 1e-9 {
		t.Errorf("sonnet cost = %v %q, want %v", got, unit, want)
	}
	auto, _ := Prices().Lookup(HarnessKiro, "auto")
	if got, unit := auto.Cost(3, 1_000_000); unit != "credits" || math.Abs(got-0.3) > 1e-9 {
		t.Errorf("kiro cost = %v %q, want 0.3 credits", got, unit)
	}
	if got := Prices().Checked(HarnessClaude, "sonnet"); got != "2026-11-27" {
		t.Errorf("sonnet's prices are as of %q", got)
	}
	if got := Prices().Checked(HarnessKiro, "sonnet"); got != "" {
		t.Errorf("an unpriced model's prices are as of %q", got)
	}
	if _, ok := Prices().Lookup(HarnessKiro, "sonnet"); ok {
		t.Error("kiro's table priced a model only claude names")
	}
}
