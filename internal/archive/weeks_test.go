package archive

import (
	"testing"
	"time"
)

func day(t *testing.T, iso string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", iso)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func weekStrings(t *testing.T, today string, n int) []string {
	t.Helper()
	var out []string
	for _, w := range RecentWeeks(n, day(t, today)) {
		out = append(out, w.String())
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMondayAndSundaySeeTheSameWeeks(t *testing.T) {
	want := []string{"2026-W45", "2026-W44", "2026-W43", "2026-W42"}
	for _, today := range []string{"2026-11-09", "2026-11-15"} {
		if got := weekStrings(t, today, 4); !equal(got, want) {
			t.Errorf("%s: %v, want %v", today, got, want)
		}
	}
}

func TestNeverTheCurrentWeek(t *testing.T) {
	for _, today := range []string{"2026-11-09", "2026-11-12", "2026-11-15"} {
		y, w := day(t, today).ISOWeek()
		cur := Week{y, w}.String()
		for _, got := range weekStrings(t, today, 1) {
			if got == cur {
				t.Errorf("%s: current week %s in the list", today, cur)
			}
		}
	}
}

func TestYearBoundaries(t *testing.T) {
	cases := map[string][]string{
		"2027-01-04": {"2026-W53", "2026-W52"},
		"2026-01-05": {"2026-W01", "2025-W52"},
	}
	for today, want := range cases {
		if got := weekStrings(t, today, 2); !equal(got, want) {
			t.Errorf("%s: %v, want %v", today, got, want)
		}
	}
	if got := weekStrings(t, "2025-12-31", 1); !equal(got, []string{"2025-W52"}) {
		t.Errorf("2025-12-31: %v", got)
	}
}

func TestParseWeek(t *testing.T) {
	if w, err := ParseWeek("2026-W45"); err != nil || w != (Week{2026, 45}) {
		t.Errorf("2026-W45 = %v, %v", w, err)
	}
	if w, err := ParseWeek("2026-w05"); err != nil || w != (Week{2026, 5}) {
		t.Errorf("2026-w05 = %v, %v", w, err)
	}
	for _, bad := range []string{"2026-45", "2025-W53", "2026-W00", "W45", ""} {
		if _, err := ParseWeek(bad); err == nil {
			t.Errorf("ParseWeek(%q) succeeded", bad)
		}
	}
}

func TestKeyIsUnderTheHarness(t *testing.T) {
	if got := Key("claude", "laptop-a", Week{2026, 5}); got != "claude/laptop-a/2026/W05.tar.zst" {
		t.Errorf("key = %s", got)
	}
}

func TestBoundsAreLocalMidnightsAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata")
	}
	start, end := Week{2026, 44}.Bounds(loc) // DST ends Sunday 2026-11-01
	if got := start.Format(time.RFC3339); got != "2026-10-26T00:00:00-07:00" {
		t.Errorf("start = %s", got)
	}
	if got := end.Format(time.RFC3339); got != "2026-11-02T00:00:00-08:00" {
		t.Errorf("end = %s", got)
	}
}

func TestParseKey(t *testing.T) {
	w := Week{2026, 45}
	h, host, got, ok := ParseKey(Key("claude", "laptop-a", w))
	if !ok || h != "claude" || host != "laptop-a" || got != w {
		t.Errorf("ParseKey(Key) = %s %s %v %v", h, host, got, ok)
	}
	for _, bad := range []string{"analysis/claude/s1-abc.json", "claude/laptop-a/.write-check", "claude/laptop-a/2026/W54.tar.zst"} {
		if _, _, _, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) ok, want not a week key", bad)
		}
	}
}
