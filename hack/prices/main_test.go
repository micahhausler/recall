package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/model"
)

var now = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

func current(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../internal/config/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRefreshWritesAValidAnswer(t *testing.T) {
	cur := current(t)
	answer := strings.Replace(refreshed(string(cur)), `"input_usd_per_mtok": 2,`, `"input_usd_per_mtok": 3,`, 1)
	f := &model.Fake{Response: "```json\n" + answer + "\n```"}
	pages := map[string]string{"https://platform.claude.com/docs/en/about-claude/pricing": "Sonnet 5.5 $3 / MTok"}
	out, err := refresh(context.Background(), f, model.Call{Model: "opus"}, cur, pages, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("%d model calls, want 1", len(f.Calls))
	}
	in := f.Calls[0].Input
	for _, want := range []string{"2026-12-01", "# PAGE https://platform.claude.com/docs/en/about-claude/pricing", "Sonnet 5.5 $3 / MTok", `"sonnet"`} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %q", want)
		}
	}
	old, _ := config.ParsePrices(cur)
	next, err := config.ParsePrices(out)
	if err != nil {
		t.Fatal(err)
	}
	diff := strings.Join(changes(old, next), "\n")
	if !strings.Contains(diff, "anthropic sonnet: $2 in / $10 out per MTok (Sonnet 5.5) -> $3 in") || !strings.Contains(diff, "checked 2026-11-27 -> 2026-12-01") {
		t.Errorf("diff = %q", diff)
	}
}

// refreshed is the table as checked today: the sourced provider's date moves.
func refreshed(table string) string {
	return strings.Replace(table, `"checked": "2026-11-27"`, `"checked": "2026-12-01"`, 1)
}

func TestRefreshRejectsABadAnswer(t *testing.T) {
	cur := string(current(t))
	good := refreshed(cur)
	for _, tc := range []struct{ name, answer, want string }{
		{"prose", "Sonnet costs $2.", "the answer"},
		{"moved source", strings.Replace(good, "/about-claude/pricing", "/pricing", 1), "source"},
		{"dropped default", strings.Replace(good, `"opus"`, `"opus-5"`, 1), "opus"},
		{"zero price", strings.Replace(good, `"output_usd_per_mtok": 10,`, `"output_usd_per_mtok": 0,`, 1), "sonnet"},
		{"kiro credits changed", strings.Replace(good, `"credits_per_call": 0.1`, `"credits_per_call": 999`, 1), "kiro"},
		{"kiro date changed", strings.Replace(good, `"checked": "2026-11-27"`, `"checked": "2026-12-01"`, 1), "kiro"},
		{"checked in the past", strings.Replace(cur, `"checked": "2026-11-27"`, `"checked": "1999-01-01"`, 1), "today"},
		{"checked in the future", strings.Replace(cur, `"checked": "2026-11-27"`, `"checked": "2099-01-01"`, 1), "today"},
		{"checked unchanged", cur, "today"},
		{"huge price", strings.Replace(good, `"input_usd_per_mtok": 2,`, `"input_usd_per_mtok": 1e300,`, 1), "by hand"},
		{"new model", strings.Replace(good, `"models": {`, `"models": {"zzz": {"input_usd_per_mtok": 1, "output_usd_per_mtok": 2},`, 1), "zzz"},
		{"key in another case", strings.Replace(good, `"source"`, `"Source"`, 1), "Source"},
		{"duplicate key", strings.Replace(good, `"name": "anthropic",`, `"name": "anthropic", "name": "anthropic",`, 1), "twice"},
		{"long note", strings.Replace(good, `"note": "Sonnet 5.5"`, `"note": "`+strings.Repeat("x", 201)+`"`, 1), "note"},
	} {
		f := &model.Fake{Response: tc.answer}
		if _, err := refresh(context.Background(), f, model.Call{}, []byte(cur), nil, now, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	added := strings.Replace(good, `"models": {`, `"models": {"zzz": {"input_usd_per_mtok": 1, "output_usd_per_mtok": 2},`, 1)
	if _, err := refresh(context.Background(), &model.Fake{Response: added}, model.Call{}, []byte(cur), nil, now, true); err != nil {
		t.Errorf("a new model with -new-models: %v", err)
	}
	f := &model.Fake{Err: errors.New("offline")}
	if _, err := refresh(context.Background(), f, model.Call{}, []byte(cur), nil, now, false); err == nil {
		t.Error("a failed call wrote a table")
	}
}

func TestSameHostRedirects(t *testing.T) {
	source, _ := url.Parse("https://platform.claude.com/docs/en/about-claude/pricing")
	check := sameHost(source)
	for _, tc := range []struct {
		to string
		ok bool
	}{
		{"https://platform.claude.com/docs/en/pricing", true},
		{"http://platform.claude.com/docs/en/pricing", false},
		{"https://example.com/pricing", false},
		{"https://platform.claude.com.example.com/pricing", false},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.to, nil)
		if err := check(req, []*http.Request{req}); (err == nil) != tc.ok {
			t.Errorf("redirect to %s: err = %v, want ok %v", tc.to, err, tc.ok)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "https://platform.claude.com/x", nil)
	if err := check(req, make([]*http.Request, 10)); err == nil {
		t.Error("an eleventh redirect was followed")
	}
}

func TestReadPageRefusesAnOversizedPage(t *testing.T) {
	if _, err := readPage(strings.NewReader(strings.Repeat("a", maxPage+1))); err == nil {
		t.Error("an oversized page was read")
	}
	if got, err := readPage(strings.NewReader("<p>Sonnet</p>")); err != nil || got != "Sonnet" {
		t.Errorf("readPage = %q, %v", got, err)
	}
}

func TestModelFor(t *testing.T) {
	if got := modelFor("kiro", ""); got != "auto" {
		t.Errorf("kiro's model = %q, want auto", got)
	}
	if got := modelFor("claude", ""); got != "opus" {
		t.Errorf("claude's model = %q, want opus", got)
	}
	if got := modelFor("kiro", "claude-opus-5.5"); got != "claude-opus-5.5" {
		t.Errorf("a named model = %q", got)
	}
}

// The checked-in table is in the form refresh writes.
func TestTableIsFormatted(t *testing.T) {
	cur := current(t)
	tbl, err := config.ParsePrices(cur)
	if err != nil {
		t.Fatal(err)
	}
	out, err := format(tbl)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(cur) {
		t.Errorf("internal/config/prices.json isn't formatted as refresh writes it:\n%s", out)
	}
}

func TestPageText(t *testing.T) {
	got := pageText("<html><head><title>x</title></head><body><script>var a=1</script><table><tr><td>Sonnet 5.5</td><td>$2 &amp; up</td></tr></table></body></html>")
	if got != "Sonnet 5.5\n$2 & up" {
		t.Errorf("pageText = %q", got)
	}
}
