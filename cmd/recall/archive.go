package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/store"
)

const archiveUsage = `Usage:
  recall archive-chats run [--lookback N] [--dry-run [--offline]]  upload missing weeks
  recall archive-chats build WEEK OUT    write one week's tarball locally (WEEK like 2026-W45)
  recall archive-chats info              resolved settings and this host's uploads
  recall archive-chats check [--setup]   region, credentials, bucket, and permissions
  recall archive-chats policy setup|upload   print the IAM policy each step needs
  recall archive-chats estimate          local transcript size, compressed size, and S3 cost
  recall archive-chats setup [--dry-run] create and configure the bucket, then verify it
`

type archiveEnv struct {
	home      string
	settings  *config.Settings
	harnesses []harness.Harness
	clk       clock.Clock
}

func loadArchiveEnv() (*archiveEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	c, err := config.Load(config.DefaultBase(home))
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	settings, err := c.Settings(home, shortHost(hostname))
	if err != nil {
		return nil, err
	}
	hs, err := harness.Enabled(settings)
	if err != nil {
		return nil, err
	}
	return &archiveEnv{home: home, settings: settings, harnesses: hs, clk: clock.Real{}}, nil
}

func (e *archiveEnv) harnessNames() []string {
	names := make([]string, 0, len(e.harnesses))
	for _, h := range e.harnesses {
		names = append(names, h.Name())
	}
	return names
}

func (e *archiveEnv) s3Only() (cfg config.S3Settings, err error) {
	if e.settings.Store.Kind != "s3" {
		return cfg, fmt.Errorf("store.kind is %q; this command manages the s3 store (the fs store needs no setup)", e.settings.Store.Kind)
	}
	return e.settings.Store.S3, nil
}

func cmdArchiveChats(args []string) int {
	fs := flag.NewFlagSet("archive-chats", flag.ContinueOnError)
	lookback := fs.Int("lookback", 0, "complete weeks to check (default: config)")
	dryRun := fs.Bool("dry-run", false, "build tarballs and report, upload nothing")
	offline := fs.Bool("offline", false, "with --dry-run: no store calls")
	setupFlag := fs.Bool("setup", false, "with check: check what setup needs")
	fs.Usage = func() { fmt.Fprint(os.Stderr, archiveUsage) }
	// Flags may follow the subcommand word; parse around it.
	cmd := "run"
	rest := args
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd = args[0]
		rest = args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *offline && !*dryRun {
		fmt.Fprintln(os.Stderr, "recall: --offline needs --dry-run")
		return 2
	}
	if *lookback < 0 {
		fmt.Fprintln(os.Stderr, "recall: --lookback must be at least 1")
		return 2
	}

	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	ctx := context.Background()
	switch cmd {
	case "run":
		return archiveRun(ctx, env, *lookback, *dryRun, *offline)
	case "build":
		return archiveBuild(env, fs.Args())
	case "info":
		return archiveInfo(ctx, env)
	case "estimate":
		return archiveEstimate(env)
	case "check":
		return archiveCheck(ctx, env, *setupFlag)
	case "policy":
		if len(fs.Args()) != 1 {
			fmt.Fprint(os.Stderr, archiveUsage)
			return 2
		}
		return archivePolicy(ctx, env, fs.Args()[0])
	case "setup":
		return archiveSetup(ctx, env, *dryRun)
	default:
		fmt.Fprint(os.Stderr, archiveUsage)
		return 2
	}
}

// openStore opens the configured store and a dest renderer for messages.
func openStore(ctx context.Context, env *archiveEnv) (store.Store, func(string) string, error) {
	st, err := store.Open(ctx, env.settings.Store)
	if err != nil {
		return nil, nil, err
	}
	dest := func(key string) string { return filepath.Join(env.settings.Store.Path, key) }
	if s3st, ok := st.(*store.S3Store); ok {
		dest = func(key string) string { return "s3://" + s3st.Bucket() + "/" + key }
	}
	return st, dest, nil
}

func archiveRun(ctx context.Context, env *archiveEnv, lookback int, dryRun, offline bool) int {
	if lookback == 0 {
		lookback = env.settings.Archive.Lookback
	}
	r := &archive.Runner{
		Harnesses: env.harnesses,
		Host:      env.settings.Host,
		Clock:     env.clk,
		Loc:       time.Local,
		Out:       os.Stdout,
		Err:       os.Stderr,
		Dest:      func(key string) string { return "s3://<bucket>/" + key },
	}
	if !offline {
		st, dest, err := openStore(ctx, env)
		if err != nil {
			return fail(err)
		}
		r.Store, r.Dest = st, dest
	} else {
		r.Store = store.NewMem()
	}
	failed, err := r.Run(ctx, lookback, dryRun, offline)
	if err != nil {
		return fail(err)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func archiveBuild(env *archiveEnv, args []string) int {
	if len(args) != 2 {
		fmt.Fprint(os.Stderr, archiveUsage)
		return 2
	}
	w, err := archive.ParseWeek(args[0])
	if err != nil {
		return fail(err)
	}
	start, end := w.Bounds(time.Local)
	total := 0
	for _, h := range env.harnesses {
		files, err := h.WeekFiles(start, end)
		if err != nil {
			return fail(err)
		}
		if len(files) == 0 {
			continue
		}
		out := args[1]
		if len(env.harnesses) > 1 {
			out = h.Name() + "-" + out
		}
		kept, size, err := archive.BuildFile(filepath.Dir(h.SourceDir()), files, out)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("%s: %d files, %d bytes\n", out, len(kept), size)
		total += len(kept)
	}
	if total == 0 {
		fmt.Fprintf(os.Stderr, "recall: %s: no changed files\n", w)
		return 1
	}
	return 0
}

func archiveInfo(ctx context.Context, env *archiveEnv) int {
	s := env.settings
	fmt.Printf("host:      %s\n", s.Host)
	fmt.Printf("store:     %s\n", s.Store.Kind)
	st, dest, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	if s.Store.Kind == "s3" {
		fmt.Printf("profile:   %s\n", orBlank(s.Store.S3.Profile, "(default credential chain)"))
		fmt.Printf("region:    %s\n", orBlank(s.Store.S3.Region, "(none)"))
	}
	fmt.Printf("objects:   %s\n", dest("<harness>/"+s.Host+"/<year>/W<week>.tar.zst"))
	fmt.Printf("lookback:  %d weeks; retention %d days\n", s.Archive.Lookback, s.Store.S3.RetentionDays)
	for _, h := range env.harnesses {
		keys, err := st.List(ctx, h.Name()+"/"+s.Host+"/")
		if err != nil {
			return fail(err)
		}
		for _, k := range keys {
			fmt.Printf("  %s\n", k)
		}
	}
	return 0
}

func orBlank(v, blank string) string {
	if v == "" {
		return blank
	}
	return v
}

func archiveEstimate(env *archiveEnv) int {
	rc := 0
	for _, h := range env.harnesses {
		e := &archive.Estimate{
			Harness:        h,
			Retention:      env.settings.Store.S3.RetentionDays,
			Lookback:       env.settings.Archive.Lookback,
			PricePerGB:     env.settings.Archive.PricePerGB,
			ClaudeSettings: filepath.Join(env.home, ".claude", "settings.json"),
			Now:            env.clk.Now(),
			Out:            os.Stdout,
		}
		hrc, err := e.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		}
		if hrc > rc {
			rc = hrc
		}
	}
	return rc
}

func archiveCheck(ctx context.Context, env *archiveEnv, setup bool) int {
	s3cfg, err := env.s3Only()
	if err != nil {
		return fail(err)
	}
	cfg, err := store.AWSConfig(ctx, s3cfg)
	if err != nil {
		return fail(err)
	}
	stsClient := sts.NewFromConfig(cfg)
	bucket, err := store.ResolveBucket(ctx, s3cfg, stsClient)
	if err != nil {
		return fail(err)
	}
	c := &archive.Check{
		Region: s3cfg.Region, Profile: s3cfg.Profile, Bucket: bucket,
		Host: env.settings.Host, Harnesses: env.harnessNames(), Setup: setup,
		STS: stsClient, S3: s3.NewFromConfig(cfg), Out: os.Stdout,
	}
	return c.Run(ctx)
}

func archivePolicy(ctx context.Context, env *archiveEnv, kind string) int {
	s3cfg, err := env.s3Only()
	if err != nil {
		return fail(err)
	}
	bucket := s3cfg.Bucket
	if bucket == "" {
		// Best effort: a placeholder when credentials aren't available.
		bucket = store.NamespacedBucket(s3cfg.BucketPrefix, "<account-id>", orBlank(s3cfg.Region, "<region>"))
		if cfg, err := store.AWSConfig(ctx, s3cfg); err == nil {
			if b, err := store.ResolveBucket(ctx, s3cfg, sts.NewFromConfig(cfg)); err == nil {
				bucket = b
			}
		}
	}
	if err := archive.PrintPolicy(os.Stdout, kind, bucket, env.settings.Host, env.harnessNames()); err != nil {
		return fail(err)
	}
	return 0
}

func archiveSetup(ctx context.Context, env *archiveEnv, dryRun bool) int {
	s3cfg, err := env.s3Only()
	if err != nil {
		return fail(err)
	}
	if s3cfg.Region == "" {
		return fail(fmt.Errorf("no region: set one with recall config set store.s3.region <region>"))
	}
	client, bucket, err := store.S3Client(ctx, s3cfg)
	if err != nil {
		return fail(err)
	}
	s := &archive.Setup{
		API: client, Bucket: bucket, Region: s3cfg.Region,
		Namespaced: s3cfg.Bucket == "", Retention: s3cfg.RetentionDays, Out: os.Stdout,
	}
	rc, err := s.Run(ctx, dryRun)
	if err != nil {
		return fail(err)
	}
	return rc
}
