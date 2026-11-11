package archive

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/harness"
)

const gib = 1 << 30

// Projection is stored bytes and monthly cost at one point in time.
type Projection struct {
	Label  string
	Months float64
	Stored float64
	USD    float64
}

// Project returns S3 Standard projections for a weekly upload rate.
func Project(weeklyBytes float64, retentionDays int, pricePerGB float64) []Projection {
	weeksPerMonth := 365.25 / 12 / 7
	cap := float64(retentionDays) / 7 * weeklyBytes
	monthsTotal := float64(retentionDays) / (365.25 / 12)
	var rows []Projection
	for _, p := range []struct {
		label  string
		months float64
	}{
		{"after 1 month", 1},
		{"after 1 year", 12},
		{fmt.Sprintf("at %d days (steady state)", retentionDays), monthsTotal},
	} {
		if p.months > monthsTotal {
			continue
		}
		stored := p.months * weeksPerMonth * weeklyBytes
		if stored > cap {
			stored = cap
		}
		rows = append(rows, Projection{p.label, p.months, stored, stored / gib * pricePerGB})
	}
	return rows
}

// TotalCost is USD for the whole retention period: storage grows linearly,
// so it averages half the cap.
func TotalCost(weeklyBytes float64, retentionDays int, pricePerGB float64) float64 {
	months := float64(retentionDays) / (365.25 / 12)
	return float64(retentionDays) / 7 * weeklyBytes / 2 / gib * pricePerGB * months
}

// CleanupDays reads cleanupPeriodDays from Claude Code's settings file;
// 30 (Claude Code's default) when unset or unreadable.
func CleanupDays(settingsPath string) int {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return 30
	}
	var s struct {
		CleanupPeriodDays *int `json:"cleanupPeriodDays"`
	}
	if json.Unmarshal(data, &s) != nil || s.CleanupPeriodDays == nil {
		return 30
	}
	return *s.CleanupPeriodDays
}

func human(n float64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if n < 1024 {
			if unit == "B" {
				return fmt.Sprintf("%.0f %s", n, unit)
			}
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1024
	}
	return fmt.Sprintf("%.1f TB", n)
}

// Estimate reports local transcript size, compressed size, and projected S3
// cost for one harness.
type Estimate struct {
	Harness        harness.Harness
	Retention      int
	Lookback       int
	PricePerGB     float64
	ClaudeSettings string
	Now            time.Time
	Out            io.Writer
}

func (e *Estimate) Run() (int, error) {
	files, err := harness.WalkFiles(e.Harness.SourceDir())
	if err != nil {
		return 1, err
	}
	if len(files) == 0 {
		return 1, fmt.Errorf("nothing to estimate: no files under %s", e.Harness.SourceDir())
	}
	var raw int64
	oldest := e.Now
	sessions := 0
	rels := make([]string, 0, len(files))
	for _, f := range files {
		raw += f.Size
		if f.ModTime.Before(oldest) {
			oldest = f.ModTime
		}
		if strings.HasSuffix(f.Rel, ".jsonl") {
			sessions++
		}
		rels = append(rels, f.Rel)
	}
	days := e.Now.Sub(oldest).Hours() / 24
	if days < 7 {
		days = 7
	}
	var packedBuf countWriter
	kept, err := Build(filepath.Dir(e.Harness.SourceDir()), rels, &packedBuf)
	if err != nil {
		return 1, err
	}
	if len(kept) == 0 {
		return 1, fmt.Errorf("nothing to estimate: the files under %s were deleted mid-run", e.Harness.SourceDir())
	}
	packed := float64(packedBuf.n)
	keep := CleanupDays(e.ClaudeSettings)
	window := days
	if float64(keep) < window {
		window = float64(keep)
	}
	if window < 7 {
		window = 7
	}
	weekly := packed / (window / 7)
	fmt.Fprintf(e.Out, "source:      %s\n", e.Harness.SourceDir())
	fmt.Fprintf(e.Out, "local:       %d files (%d .jsonl transcripts), %s, oldest %.0f days ago\n",
		len(files), sessions, human(float64(raw)), days)
	fmt.Fprintf(e.Out, "compressed:  %s with zstd -3 --long=27 (%.1fx)\n", human(packed), float64(raw)/maxf(packed, 1))
	fmt.Fprintf(e.Out, "per week:    about %s compressed, averaged over the last %.0f days\n", human(weekly), window)
	fmt.Fprintf(e.Out, "S3 Standard at $%v/GB-month, one machine, %d-day retention:\n", e.PricePerGB, e.Retention)
	for _, row := range Project(weekly, e.Retention, e.PricePerGB) {
		fmt.Fprintf(e.Out, "  %-34s %10s stored  $%.4f/month\n", row.Label, human(row.Stored), row.USD)
	}
	fmt.Fprintf(e.Out, "  %-34s %10s         $%.2f\n", "total over the retention period", "", TotalCost(weekly, e.Retention, e.PricePerGB))
	fmt.Fprintln(e.Out, "  Requests (about 4 PUTs a month) cost well under a cent. Rough: a session active across")
	fmt.Fprintln(e.Out, "  weeks is stored once per week it changed, and prices vary by region: aws.amazon.com/s3/pricing")
	if keep < (e.Lookback+1)*7 {
		fmt.Fprintf(e.Out, "warning: Claude Code deletes transcripts after %d days (cleanupPeriodDays), but a run"+
			" catches up %d weeks back. Set cleanupPeriodDays to at least %d"+
			" in %s so a missed week isn't lost.\n", keep, e.Lookback, (e.Lookback+1)*7, e.ClaudeSettings)
	}
	return 0, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
