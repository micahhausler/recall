package archive

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectionGrowsThenCaps(t *testing.T) {
	rows := Project(gib, 730, 0.023) // 1 GiB a week, two years kept
	labels := []string{"after 1 month", "after 1 year", "at 730 days (steady state)"}
	for i, want := range labels {
		if rows[i].Label != want {
			t.Errorf("row %d = %s, want %s", i, rows[i].Label, want)
		}
	}
	weeksPerMonth := 365.25 / 12 / 7
	for i, want := range []float64{weeksPerMonth, 12 * weeksPerMonth, 730.0 / 7} {
		if got := rows[i].Stored / gib; math.Abs(got-want) > 1e-6 {
			t.Errorf("row %d stored = %f GiB, want %f", i, got, want)
		}
	}
	if got, want := rows[2].USD, 730.0/7*0.023; math.Abs(got-want) > 1e-6 {
		t.Errorf("steady state = $%f, want $%f", got, want)
	}
}

func TestShortRetentionSkipsLongerRows(t *testing.T) {
	rows := Project(gib, 90, 0.023)
	if len(rows) != 2 || rows[1].Label != "at 90 days (steady state)" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestTotalIsHalfTheCapOverThePeriod(t *testing.T) {
	months := 365 / (365.25 / 12)
	if got, want := TotalCost(gib, 365, 1.0), 365.0/7/2*months; math.Abs(got-want) > 1e-6 {
		t.Errorf("total = %f, want %f", got, want)
	}
}

func TestCleanupDaysReadsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if got := CleanupDays(path); got != 30 {
		t.Errorf("missing file: %d, want the 30-day default", got)
	}
	os.WriteFile(path, []byte(`{"cleanupPeriodDays": 400}`), 0o644)
	if got := CleanupDays(path); got != 400 {
		t.Errorf("got %d, want 400", got)
	}
	os.WriteFile(path, []byte(`not json`), 0o644)
	if got := CleanupDays(path); got != 30 {
		t.Errorf("bad json: %d, want 30", got)
	}
}

func TestEstimateReportAndCleanupWarning(t *testing.T) {
	tr := newTree(t)
	now := time.Now()
	tr.touch("p/a.jsonl", now.AddDate(0, 0, -200), strings.Repeat("x", 5000))
	tr.touch("p/b.jsonl", now, strings.Repeat("y", 5000))
	settings := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(settings, []byte(`{"cleanupPeriodDays": 30}`), 0o644)
	out := &strings.Builder{}
	e := &Estimate{
		Harness: tr.harness(t), Retention: 3650, Lookback: 4, PricePerGB: 0.023,
		ClaudeSettings: settings, Now: now, Out: out,
	}
	rc, err := e.Run()
	if err != nil || rc != 0 {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	for _, want := range []string{
		"2 files (2 .jsonl transcripts)",
		"averaged over the last 30 days", // capped at cleanupPeriodDays, not 200
		"total over the retention period",
		"Set cleanupPeriodDays to at least 35",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}

	os.WriteFile(settings, []byte(`{"cleanupPeriodDays": 365}`), 0o644)
	out.Reset()
	if _, err := e.Run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "cleanupPeriodDays") {
		t.Errorf("long retention still warned:\n%s", out)
	}
}

func TestEstimateNothingToEstimate(t *testing.T) {
	tr := newTree(t)
	e := &Estimate{Harness: tr.harness(t), Retention: 3650, Lookback: 4, PricePerGB: 0.023,
		Now: time.Now(), Out: &strings.Builder{}}
	if rc, err := e.Run(); err == nil || rc != 1 {
		t.Errorf("rc=%d err=%v, want 1 and an error", rc, err)
	}
}
