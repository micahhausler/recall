package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/imports"
)

const importUsage = `Usage:
  recall import [--full] [--replace] [--dry-run] [NAME...]

Runs the plugin each named harness's [harness.NAME.import] table names, and
stores the sessions it writes as that harness's (docs/import.md). With no
NAME, every enabled harness with an import table.

  --full     ask the plugin for everything, not only what changed
  --replace  accept a copy of a stored session with fewer events
  --dry-run  run the plugin and report what would change; store nothing
`

// importFlags are recall import's flags.
type importFlags struct {
	full, replace, dryRun bool
}

func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	var f importFlags
	fs.BoolVar(&f.full, "full", false, "ask for everything")
	fs.BoolVar(&f.replace, "replace", false, "accept shorter copies of stored sessions")
	fs.BoolVar(&f.dryRun, "dry-run", false, "store nothing")
	fs.Usage = func() { fmt.Fprint(os.Stderr, importUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	return runImports(context.Background(), env, fs.Args(), f, execSource)
}

// execSource runs an import's configured command, streaming its stderr.
func execSource(name string, imp *config.ImportSettings) imports.Source {
	return &imports.Exec{Command: imp.Command, Timeout: imp.Timeout, Stderr: os.Stderr, Prefix: name + ": "}
}

// runImports runs each named import, or every configured one, through the
// Source newSource opens, and exits non-zero when any failed.
func runImports(ctx context.Context, env *archiveEnv, names []string, f importFlags,
	newSource func(name string, imp *config.ImportSettings) imports.Source) int {
	if len(names) == 0 {
		if names = importNames(env); len(names) == 0 {
			fmt.Println("import: no enabled harness has an [harness.<name>.import] table")
			return 0
		}
	}
	failed := 0
	for _, name := range names {
		if err := runImport(ctx, env, name, f, newSource); err != nil {
			fmt.Fprintf(os.Stderr, "recall: import %s: %v\n", name, err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// importNames are the enabled harnesses with an import table, sorted.
func importNames(env *archiveEnv) []string {
	var names []string
	for name, h := range env.settings.Harnesses {
		if h.Import != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func runImport(ctx context.Context, env *archiveEnv, name string, f importFlags,
	newSource func(name string, imp *config.ImportSettings) imports.Source) error {
	h, ok := env.settings.Harnesses[name]
	switch {
	case !ok:
		return fmt.Errorf("no enabled harness %q", name)
	case h.Import == nil:
		return fmt.Errorf("harness %q has no [harness.%s.import] table, so there is no plugin to run", name, name)
	}
	warnOtherImporters(ctx, env, name)
	res, err := imports.Run(ctx, newSource(name, h.Import), imports.Options{
		Name: name, Host: env.settings.Host, Dir: config.ImportDir(env.home, name), Sessions: h.Source,
		Full: f.full, Replace: f.replace, DryRun: f.dryRun,
		Now: env.clk.Now(), LockStale: h.Import.Timeout + time.Hour,
	})
	if err != nil {
		if errors.Is(err, imports.ErrLocked) {
			return err
		}
		return fmt.Errorf("%w; nothing was stored, and the next run asks from the same point", err)
	}
	reportImport(name, res, f.dryRun)
	return nil
}

// reportImport prints one run's outcome: a summary line on stdout, and on
// stderr everything the owner should look at.
func reportImport(name string, res *imports.Result, dryRun bool) {
	warn := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "recall: import %s: %s\n", name, fmt.Sprintf(format, args...))
	}
	for _, s := range res.Skipped {
		warn("skipped %s: not a regular *.jsonl file", s)
	}
	for _, r := range res.Rejected {
		warn("rejected %s: %s", r.File, r.Reason)
	}
	for _, held := range res.Held {
		warn("held %s: the new copy has %d fewer event(s) than the stored one; --replace accepts it", held.ID, held.Drop)
	}
	if res.Dropped > 0 {
		warn("dropped %d event(s) of a kind this recall doesn't know; a newer recall reads them", res.Dropped)
	}
	if res.Status.Sessions != res.Found {
		warn("the plugin said it wrote %d session(s), and recall found %d", res.Status.Sessions, res.Found)
	}
	if res.Status.Partial {
		warn("the plugin fetched only part of what it was asked for; the next run asks from the same point")
	}
	verb, label := "stored", "import "+name
	if dryRun {
		verb, label = "would store", label+" --dry-run"
	}
	parts := []string{
		fmt.Sprintf("%d session(s) found", res.Found),
		fmt.Sprintf("%d %s", len(res.Stored), verb),
		fmt.Sprintf("%d unchanged", res.Unchanged),
	}
	if len(res.Held) > 0 {
		parts = append(parts, fmt.Sprintf("%d held", len(res.Held)))
	}
	if len(res.Rejected) > 0 {
		parts = append(parts, fmt.Sprintf("%d rejected", len(res.Rejected)))
	}
	if res.Status.Filtered > 0 {
		parts = append(parts, fmt.Sprintf("%d filtered by the plugin", res.Status.Filtered))
	}
	if res.Redactions > 0 {
		parts = append(parts, fmt.Sprintf("%d token(s) redacted", res.Redactions))
	}
	since := "everything"
	if res.Since != "" {
		since = "changes since " + res.Since
	}
	fmt.Printf("%s: asked for %s; %s\n", label, since, strings.Join(parts, ", "))
}

// warnOtherImporters warns when another host has archived this harness
// within the archive's lookback: one source imported on two machines
// counts each session twice. A store that can't be listed says nothing.
func warnOtherImporters(ctx context.Context, env *archiveEnv, name string) {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return
	}
	keys, err := st.List(ctx, name+"/")
	if err != nil {
		return
	}
	cutoff := env.clk.Now().AddDate(0, 0, -7*(archive.DefaultLookback+1))
	hosts := map[string]bool{}
	for _, key := range keys {
		h, host, w, ok := archive.ParseKey(key)
		if !ok || h != name || host == env.settings.Host || w.Start(time.UTC).Before(cutoff) {
			continue
		}
		hosts[host] = true
	}
	others := make([]string, 0, len(hosts))
	for host := range hosts {
		others = append(others, host)
	}
	sort.Strings(others)
	if len(others) > 0 {
		fmt.Fprintf(os.Stderr, "recall: import %s: %s archived this harness recently too; import each source on one machine, or its sessions count twice\n",
			name, strings.Join(others, ", "))
	}
}
