package learn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

// Phase 2 gate: a lesson whose sessions all ran in one repo reaches that
// repo's own CLAUDE.md, through the whole loop on fakes: reduce, plan,
// apply with the fake editor, and the branch publisher on a temp git repo.
// The commit touches only that repo's CLAUDE.md, and the proposal cites
// only that repo's sessions.
func TestPhase2GateRepoLessonEndToEnd(t *testing.T) {
	ctx := context.Background()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	alpha := liveRepo(t, base, "alpha", map[string]string{"CLAUDE.md": "# Alpha\n\n- one\n"})
	beta := liveRepo(t, base, "beta", map[string]string{"CLAUDE.md": "# Beta\n\n- two\n"})
	repoA, err := DiscoverRepo(alpha)
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := DiscoverRepo(beta)
	if err != nil {
		t.Fatal(err)
	}

	// Two sessions in alpha share one lesson; one session in beta has its
	// own. All carry words the owner typed.
	s1 := session("s1", "2026-11-01", zshA)
	s1.Scope = Scope{Repo: "~/alpha"}
	s1.Observations[0].Source = SourceOwner
	s2 := session("s2", "2026-11-02", zshA)
	s2.Scope = Scope{Repo: "~/alpha"}
	s2.Observations[0].Source = SourceOwner
	s3 := session("s3", "2026-11-03", "Backups must never delete data before the upload is confirmed.")
	s3.Scope = Scope{Repo: "~/beta"}
	s3.Observations[0].Source = SourceOwner
	window := []SessionObservations{s1, s2, s3}

	rule := "Quote words that begin with an equals sign."
	target := filepath.Join(alpha, "CLAUDE.md")
	fake := &model.Fake{Response: "## Proposals\n\n" +
		"### add: Quote separators\n- target: " + target + "\n- rule: " + rule + "\n- evidence: c-0001\n"}
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	r := &Reducer{Runner: fake, Ledger: &Ledger{}, Repos: []Repo{repoA, repoB}, Home: base,
		Clock: fakeClock, RequireOwnerEvidence: true}
	dir := filepath.Join(base, "state", "2026-11-11")
	path, _, err := r.Run(ctx, window, dir)
	if err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	proposals := string(md)
	// The proposal cites only alpha's sessions, and nothing held it back.
	if !strings.Contains(proposals, "s1") || !strings.Contains(proposals, "s2") || strings.Contains(proposals, "s3") {
		t.Errorf("the proposal should cite s1 and s2 and never s3:\n%s", proposals)
	}
	for _, section := range []string{"Scope problems", "Unsupported proposals", "Off-target"} {
		if strings.Contains(proposals, section) {
			t.Errorf("nothing should be flagged under %q:\n%s", section, proposals)
		}
	}
	ev, err := LoadRunEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := ev.sessions([]string{"c-0001"})
	if len(refs) != 2 {
		t.Fatalf("evidence for c-0001 = %+v, want s1 and s2", refs)
	}
	for _, ref := range refs {
		if ref.Repo != "~/alpha" {
			t.Errorf("session %s recorded in %q, want ~/alpha", ref.Session, ref.Repo)
		}
	}

	editor := &model.FakeEditor{Apply: func(input string, dirs []string) (string, error) {
		var at string
		for _, line := range strings.Split(input, "\n") {
			if p, ok := strings.CutPrefix(line, "- edit: "); ok {
				at = p
			}
		}
		data, err := os.ReadFile(at)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(at, append(data, []byte("- "+rule+"\n")...), 0o644); err != nil {
			return "", err
		}
		return "Applied[add]: Quote separators -- " + at, nil
	}}
	a := &Applier{Editor: editor, Model: "sonnet", Publisher: review.Branch{},
		Repos: []Repo{repoA, repoB}, Home: base, Clock: fakeClock, RequireOwnerEvidence: true}
	plan, err := a.Plan(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Skip != "" {
		t.Fatalf("routes = %+v", plan.Routes)
	}
	if got := strings.Join(plan.Routes[0].Edit, ","); got != target {
		t.Errorf("route edits %s, want only %s", got, target)
	}
	if strings.Join(plan.Repos, ",") != alpha {
		t.Errorf("plan repos = %v, want only alpha", plan.Repos)
	}

	res, err := a.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 {
		t.Fatalf("changes = %+v", res.Changes)
	}
	ch := res.Changes[0]
	if ch.Error != "" || ch.Repo != alpha || ch.Where != "branch "+plan.Branch {
		t.Errorf("landed = %+v", ch)
	}
	if got := ch.Landed[plan.Routes[0].ID]; got != rule {
		t.Errorf("landed rule = %q, want %q", got, rule)
	}

	// The commit touches only alpha's CLAUDE.md; beta and the live files
	// stay as they were.
	if names := gitIn(t, alpha, "show", "--name-only", "--format=", plan.Branch); names != "CLAUDE.md" {
		t.Errorf("the branch commit touches %q, want only CLAUDE.md", names)
	}
	if got := gitIn(t, alpha, "show", plan.Branch+":CLAUDE.md"); !strings.Contains(got, rule) || !strings.Contains(got, "- one") {
		t.Errorf("branch CLAUDE.md = %q", got)
	}
	if out := gitIn(t, beta, "branch", "--list", "learn-*"); out != "" {
		t.Errorf("beta grew a learn branch: %s", out)
	}
	if live, _ := os.ReadFile(target); string(live) != "# Alpha\n\n- one\n" {
		t.Errorf("the live file changed before review: %q", live)
	}
	if live, _ := os.ReadFile(filepath.Join(beta, "CLAUDE.md")); string(live) != "# Beta\n\n- two\n" {
		t.Errorf("beta's live file changed: %q", live)
	}
}
