package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/store"
)

// Runner uploads missing weeks for every harness.
type Runner struct {
	Store     store.Store
	Harnesses []harness.Harness
	Host      string
	Clock     clock.Clock
	Loc       *time.Location
	// Dest renders an object key as a destination for messages,
	// e.g. s3://bucket/<key>.
	Dest func(key string) string
	Out  io.Writer
	Err  io.Writer
}

// Run checks the last lookback complete weeks and uploads any that are
// missing for this host. Returns the number of failed uploads.
func (r *Runner) Run(ctx context.Context, lookback int, dryRun, offline bool) (int, error) {
	weeks := RecentWeeks(lookback, r.Clock.Now().In(r.Loc))
	failed := 0
	for _, h := range r.Harnesses {
		have := map[string]bool{}
		if !offline {
			keys, err := r.Store.List(ctx, h.Name()+"/"+r.Host+"/")
			if err != nil {
				return 0, err
			}
			for _, k := range keys {
				have[k] = true
			}
		}
		for _, w := range weeks {
			key := Key(h.Name(), r.Host, w)
			if have[key] {
				continue
			}
			n, err := r.one(ctx, h, w, key, dryRun, offline)
			if err != nil {
				fmt.Fprintf(r.Err, "%s: upload failed: %v\n", w, err)
				failed++
			} else if n != "" {
				fmt.Fprintln(r.Out, n)
			}
		}
	}
	return failed, nil
}

func (r *Runner) one(ctx context.Context, h harness.Harness, w Week, key string, dryRun, offline bool) (string, error) {
	start, end := w.Bounds(r.Loc)
	files, err := h.WeekFiles(start, end)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return fmt.Sprintf("%s: no changed files, nothing to upload", w), nil
	}
	tmp, err := os.MkdirTemp("", "recall-archive.")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "a.tar.zst")
	files, size, err := BuildFile(filepath.Dir(h.SourceDir()), files, out)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return fmt.Sprintf("%s: the changed files were deleted before archiving; nothing to upload", w), nil
	}
	dest := r.Dest(key)
	if dryRun {
		return fmt.Sprintf("%s: would upload %d files, %d bytes to %s", w, len(files), size, dest), nil
	}
	f, err := os.Open(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	err = r.Store.PutIfAbsent(ctx, key, f)
	if errors.Is(err, store.ErrExists) {
		return fmt.Sprintf("%s: already in %s", w, dest), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: uploaded %d files, %d bytes to %s", w, len(files), size, dest), nil
}
