package config

import (
	"runtime"
	"strings"
	"testing"
)

// Each constant-now key loads, is ignored, and warns with what replaced it.
func TestRemovedConstantKeysWarn(t *testing.T) {
	for key, tc := range map[string]struct{ toml, want string }{
		"learn.ledger":                 {"[learn]\nledger = \"/l.json\"\n", "~/.local/state/recall/ledger.json"},
		"learn.candidates":             {"[learn]\ncandidates = \"/c.json\"\n", "~/.local/state/recall/learn/candidates.json"},
		"recur.prefix":                 {"[recur]\nprefix = \"my.jobs\"\n", "fixed at recall.recur"},
		"archive-chats.price_per_gb":   {"[archive-chats]\nprice_per_gb = 0.05\n", "$0.023/GB-month"},
		"learn.require_owner_evidence": {"[learn]\nrequire_owner_evidence = false\n", "always on"},
		"learn.window_days":            {"[learn]\nwindow_days = 365\n", "--window 365d"},
		"archive-chats.lookback":       {"[archive-chats]\nlookback = 8\n", "--lookback"},
		"store.s3.bucket_prefix":       {"[store.s3]\nbucket_prefix = \"chat-archive\"\n", "full name in store.s3.bucket"},
	} {
		b := base(t)
		write(t, b, "host = \"laptop-a\"\n"+tc.toml)
		c, err := Load(b)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if _, ok := c.Get(key); ok {
			t.Errorf("%s: Get still finds the ignored key", key)
		}
		ws := c.Warnings()
		if len(ws) != 1 {
			t.Fatalf("%s: warnings = %v, want one", key, ws)
		}
		for _, want := range []string{"config.toml:3", `"` + key + `"`, "ignored", tc.want} {
			if !strings.Contains(ws[0].String(), want) {
				t.Errorf("%s: warning %q doesn't mention %s", key, ws[0], want)
			}
		}
		if _, err := c.Settings("/home/you", "laptop-a"); err != nil {
			t.Errorf("%s: settings: %v", key, err)
		}
	}
}

// A custom recur.prefix strands its old jobs, so the warning prints the cleanup.
func TestRemovedRecurPrefixPrintsCleanup(t *testing.T) {
	b := base(t)
	write(t, b, "[recur]\nprefix = \"my.jobs\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	ws := c.Warnings()
	if len(ws) != 1 || !strings.Contains(ws[0].String(), recurPrefixCleanup("my.jobs", runtime.GOOS)) {
		t.Errorf("warnings = %v, want the cleanup for my.jobs", ws)
	}
	for goos, want := range map[string]string{
		"linux":  `crontab -l | sed '/^# BEGIN recall-recur my\.jobs /,/^# END recall-recur my\.jobs$/d' | crontab -`,
		"darwin": `for f in ~/Library/LaunchAgents/my.jobs.*.plist; do launchctl bootout gui/$(id -u) "$f"; rm "$f"; done`,
	} {
		got := recurPrefixCleanup("my.jobs", goos)
		if !strings.HasSuffix(got, ": "+want) {
			t.Errorf("%s: cleanup %q doesn't end with %q", goos, got, want)
		}
		if !strings.Contains(got, "before running recall recur sync") {
			t.Errorf("%s: cleanup %q doesn't come before the sync", goos, got)
		}
	}
}

// A prefix the old binary refused, or one that overlaps the fixed prefix,
// gets no command to paste.
func TestRemovedRecurPrefixNoCommand(t *testing.T) {
	for prefix, why := range map[string]string{
		RecurPrefix:              "",
		"":                       "",
		"x' ; echo INJECTED ; '": "never installed",
		"my jobs":                "never installed",
		"recall":                 "overlaps",
		"recall.recur.extra":     "overlaps",
	} {
		for _, goos := range []string{"linux", "darwin"} {
			got := recurPrefixCleanup(prefix, goos)
			for _, bad := range []string{"crontab", "launchctl", "rm ", "INJECTED"} {
				if strings.Contains(got, bad) {
					t.Errorf("%q on %s: cleanup %q prints %q", prefix, goos, got, bad)
				}
			}
			if why != "" && !strings.Contains(got, why) {
				t.Errorf("%q on %s: cleanup %q doesn't say why (%s)", prefix, goos, got, why)
			}
		}
	}
}

func TestStatePathsUnderHome(t *testing.T) {
	if got := LedgerPath("/home/you"); got != "/home/you/.local/state/recall/ledger.json" {
		t.Errorf("ledger = %q", got)
	}
	if got := CandidatesPath("/home/you"); got != "/home/you/.local/state/recall/learn/candidates.json" {
		t.Errorf("candidates = %q", got)
	}
}

func TestSchemaPatternCount(t *testing.T) {
	if len(schema) != 52 {
		t.Errorf("schema has %d patterns, want 52", len(schema))
	}
}

// An s3 store names its bucket; the load error says how to set one.
func TestS3StoreNeedsBucket(t *testing.T) {
	b := base(t)
	write(t, b, "[store]\nkind = \"s3\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Settings("/home/you", "laptop-a")
	if err == nil {
		t.Fatal("an s3 store with no bucket loaded")
	}
	for _, want := range []string{"config.toml:2", "store.s3.bucket", "recall config set store.s3.bucket <name>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't mention %s", err, want)
		}
	}
	write(t, b, "[store]\nkind = \"s3\"\n[store.s3]\nbucket = \"bkt\"\n")
	if c, err = Load(b); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Settings("/home/you", "laptop-a"); err != nil || s.Store.S3.Bucket != "bkt" {
		t.Errorf("settings = %+v, %v", s, err)
	}
}
