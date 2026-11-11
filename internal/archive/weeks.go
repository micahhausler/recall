// Package archive uploads each week's transcripts to the store, one zstd
// tarball per harness per machine per ISO week:
//
//	<harness>/<host>/<ISO year>/W<week>.tar.zst
//
// A week runs Monday 00:00 to the next Monday 00:00, local time. Transcripts
// are append-only, so a session that spans weeks lands in each week it was
// touched, and the newest copy of a path is the complete one. Each run checks
// the last `lookback` complete weeks, never the current one, so a missed run
// is caught up by the next.
package archive

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Week is one ISO week.
type Week struct {
	Year int
	Num  int
}

func (w Week) String() string {
	return fmt.Sprintf("%d-W%02d", w.Year, w.Num)
}

// Key is the object key for one harness, host, and week.
func Key(harness, host string, w Week) string {
	return fmt.Sprintf("%s/%s/%d/W%02d.tar.zst", harness, host, w.Year, w.Num)
}

// Start returns the week's local Monday midnight. A date's own UTC offset
// applies, so weeks that cross a DST change have the right bounds.
func (w Week) Start(loc *time.Location) time.Time {
	// Jan 4 is always in ISO week 1.
	jan4 := time.Date(w.Year, 1, 4, 0, 0, 0, 0, loc)
	monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
	return monday.AddDate(0, 0, (w.Num-1)*7)
}

// Bounds returns the half-open [start, end) of the week, local midnights.
func (w Week) Bounds(loc *time.Location) (time.Time, time.Time) {
	start := w.Start(loc)
	return start, start.AddDate(0, 0, 7)
}

// RecentWeeks returns the n complete ISO weeks before today's, newest first.
func RecentWeeks(n int, today time.Time) []Week {
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	out := make([]Week, 0, n)
	for k := 1; k <= n; k++ {
		y, w := monday.AddDate(0, 0, -7*k).ISOWeek()
		out = append(out, Week{y, w})
	}
	return out
}

var weekRE = regexp.MustCompile(`^(\d{4})-[Ww](\d{2})$`)

// ParseWeek reads YYYY-Www, like 2026-W45.
func ParseWeek(text string) (Week, error) {
	m := weekRE.FindStringSubmatch(text)
	if m == nil {
		return Week{}, fmt.Errorf("bad week %q; want YYYY-Www, like 2026-W45", text)
	}
	y, _ := strconv.Atoi(m[1])
	n, _ := strconv.Atoi(m[2])
	w := Week{y, n}
	// A year has 52 or 53 ISO weeks; reject a week its year doesn't have.
	if gotY, gotW := w.Start(time.UTC).ISOWeek(); n < 1 || gotY != y || gotW != n {
		return Week{}, fmt.Errorf("bad week %q: %d has no week %d", text, y, n)
	}
	return w, nil
}
