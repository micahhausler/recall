package learn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

func TestParseEnforcement(t *testing.T) {
	for _, tc := range []struct {
		enforce, hook, permission string
		want                      Enforcement
		why                       string
	}{
		{"", "", "", Enforcement{Kind: "script"}, ""},
		{"script", "", "", Enforcement{Kind: "script"}, ""},
		{"hook", "`PreToolUse Bash`", "", Enforcement{Kind: "hook", Event: "PreToolUse", Matcher: "Bash"}, ""},
		{"hook", "Stop", "", Enforcement{Kind: "hook", Event: "Stop"}, ""},
		{"hook", "SessionStart", "", Enforcement{}, "isn't PreToolUse"},
		{"hook", "PreToolUse Bash; rm -rf", "", Enforcement{}, "isn't an event and a tool matcher"},
		{"hook", "PreToolUse $(x)", "", Enforcement{}, "isn't a tool name pattern"},
		{"permission", "", "ask Bash(git push:*)", Enforcement{Kind: "permission", Verb: "ask", Rule: "Bash(git push:*)"}, ""},
		{"permission", "", "deny WebFetch", Enforcement{Kind: "permission", Verb: "deny", Rule: "WebFetch"}, ""},
		{"permission", "", "allow Bash(*)", Enforcement{}, "never allow"},
		{"permission", "", "Bash(git push:*)", Enforcement{}, "doesn't start with deny or ask"},
		{"daemon", "", "", Enforcement{}, "isn't script, hook, or permission"},
	} {
		got, why := parseEnforcement(tc.enforce, tc.hook, tc.permission)
		if tc.why != "" {
			if !strings.Contains(why, tc.why) {
				t.Errorf("%q %q %q: why = %q, want %q", tc.enforce, tc.hook, tc.permission, why, tc.why)
			}
			continue
		}
		if why != "" || got != tc.want {
			t.Errorf("%q %q %q = %+v, %q; want %+v", tc.enforce, tc.hook, tc.permission, got, why, tc.want)
		}
	}
}

const settingsBefore = `{
  "model": "opus",
  "permissions": {
    "allow": ["Bash(go test:*)", "Read"],
    "deny": [
      "Bash(rm -rf:*)"
    ]
  },
  "env": {"A": "1 && 2 <x>"},
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Write",
        "hooks": [{"type": "command", "command": "lint-on-write"}]
      }
    ]
  }
}
`

// The binary's edit adds exactly what was asked and leaves every other byte
// of the owner's settings as written, in its order.
func TestEnforceEditsSettingsInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(settingsBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := Enforcement{Kind: "hook", Event: "PreToolUse", Matcher: "Bash"}
	perm := Enforcement{Kind: "permission", Verb: "ask", Rule: "Bash(git push:*)"}
	for _, e := range []Enforcement{hook, perm, hook, perm} { // twice: adding again changes nothing
		if err := Enforce(path, e, `"$HOME"/bin/check-push`); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	want := `{
  "model": "opus",
  "permissions": {
    "allow": ["Bash(go test:*)", "Read"],
    "deny": [
      "Bash(rm -rf:*)"
    ],
    "ask": [
      "Bash(git push:*)"
    ]
  },
  "env": {"A": "1 && 2 <x>"},
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Write",
        "hooks": [{"type": "command", "command": "lint-on-write"}]
      },
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "\"$HOME\"/bin/check-push"
          }
        ]
      }
    ]
  }
}
`
	if got != want {
		t.Errorf("settings after:\n%s\nwant:\n%s", got, want)
	}
	if _, err := parseObject(data); err != nil {
		t.Errorf("the result isn't a JSON object: %v", err)
	}

	// A missing file is created; a file that isn't an object is refused.
	fresh := filepath.Join(t.TempDir(), "sub", "settings.json")
	if err := Enforce(fresh, Enforcement{Kind: "hook", Event: "Stop"}, "/bin/x"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(fresh); !strings.Contains(string(data), `"Stop": [`) || strings.Contains(string(data), "matcher") {
		t.Errorf("new settings = %s", data)
	}
	for name, content := range map[string]string{
		"not an object":            "[1, 2]",
		"content after the object": "{\"a\": 1}\n{\"b\": 2}\n",
		"permissions isn't a map":  "{\"permissions\": null}",
	} {
		bad := filepath.Join(t.TempDir(), "settings.json")
		os.WriteFile(bad, []byte(content), 0o644)
		if err := Enforce(bad, perm, ""); err == nil {
			t.Errorf("%s: the file was edited", name)
		}
		if after, _ := os.ReadFile(bad); string(after) != content {
			t.Errorf("%s: a refused edit changed the file", name)
		}
	}

	// The one function that writes settings refuses what a proposal may
	// not ask for, whoever calls it.
	clean := filepath.Join(t.TempDir(), "settings.json")
	for _, e := range []Enforcement{
		{Kind: "permission", Verb: "allow", Rule: "Bash(*)"},
		{Kind: "hook", Event: "SessionStart"},
		{Kind: "hook", Event: "PreToolUse", Matcher: "Bash; rm"},
	} {
		if err := Enforce(clean, e, "/bin/x"); err == nil {
			t.Errorf("Enforce wrote %+v", e)
		}
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) {
		t.Error("a refused edit still wrote the file")
	}

	// With a key written twice, the edit lands on the one a parser keeps.
	twice := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(twice, []byte("{\n  \"permissions\": {\"deny\": [\"A\"]},\n  \"permissions\": {\"deny\": [\"B\"]}\n}\n"), 0o644)
	if err := Enforce(twice, Enforcement{Kind: "permission", Verb: "deny", Rule: "WebFetch"}, ""); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Permissions struct{ Deny []string } `json:"permissions"`
	}
	data, _ = os.ReadFile(twice)
	if err := json.Unmarshal(data, &parsed); err != nil || strings.Join(parsed.Permissions.Deny, ",") != "B,WebFetch" {
		t.Errorf("after an edit a parser reads deny = %v, %v:\n%s", parsed.Permissions.Deny, err, data)
	}
}

const enforcedProposals = `## Proposals

### graduate: Ask before a push
- replaces: r-0007
- enforce: permission
- permission: ask Bash(git push:*)
- rule: no script
- evidence: r-0007

### graduate: Block force pushes
- target: ~/kit/bin/no-force-push
- replaces: r-0008
- enforce: hook
- hook: PreToolUse Bash
- rule: a script sketch
- evidence: r-0008

### graduate: Grant itself everything
- enforce: permission
- permission: allow Bash(*)
- rule: none
`

func TestApplyGraduatesIntoSettings(t *testing.T) {
	f := newApplyFixture(t, enforcedProposals)
	ctx := context.Background()
	settings := filepath.Join(f.dot, ".claude", "settings.json")

	// Without learn.settings there is nowhere to put either.
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Routes[:2] {
		if !strings.Contains(r.Skip, "learn.settings isn't set") {
			t.Errorf("%q without a settings file: %q", r.Title, r.Skip)
		}
	}

	writeFile(t, settings, "{\n  \"model\": \"opus\"\n}\n")
	gitIn(t, f.dot, "add", "-A")
	gitIn(t, f.dot, "commit", "-q", "-m", "settings")
	gitIn(t, f.dot, "push", "-q", "origin", "HEAD:main")
	f.applier.Settings = settings
	plan, err = f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Routes[0].Skip != "" || plan.Routes[1].Skip != "" {
		t.Fatalf("skips: %q, %q", plan.Routes[0].Skip, plan.Routes[1].Skip)
	}
	if !strings.Contains(plan.Routes[2].Skip, "never allow") {
		t.Errorf("an allow rule wasn't refused: %q", plan.Routes[2].Skip)
	}

	// The model writes the script and drops the prose; it never touches
	// the settings file.
	scripts := fakeApply("#!/bin/sh")
	f.editor.Apply = func(input string, dirs []string) (string, error) {
		out, err := scripts(input, dirs)
		claude := filepath.Join(dirs[0], ".claude", "CLAUDE.md")
		if werr := os.WriteFile(claude, []byte("# Rules\n"), 0o644); werr != nil {
			return "", werr
		}
		return out + "\nApplied[graduate]: Ask before a push -- " + claude, err
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	in := f.editor.Calls[0].Input
	for _, want := range []string{
		"- enforced: the binary adds this script to the harness settings as a PreToolUse Bash hook",
		`- enforced: the binary adds the permission rule "ask Bash(git push:*)"`,
	} {
		if !strings.Contains(in, want) {
			t.Errorf("apply input misses %q", want)
		}
	}
	got := gitIn(t, f.dot, "show", plan.Branch+":.claude/settings.json")
	for _, want := range []string{`"model": "opus"`, `"ask": [`, `"Bash(git push:*)"`, `"matcher": "Bash"`, `"command": "\"$HOME\"/kit/bin/no-force-push"`} {
		if !strings.Contains(got, want) {
			t.Errorf("settings on the branch miss %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "allow") {
		t.Errorf("an allow rule reached the settings:\n%s", got)
	}
	if live, _ := os.ReadFile(settings); string(live) != "{\n  \"model\": \"opus\"\n}\n" {
		t.Errorf("the live settings file changed: %s", live)
	}
	applied := 0
	for _, o := range res.Outcomes {
		if o.Applied {
			applied++
		}
	}
	if applied != 2 {
		t.Errorf("outcomes = %+v", res.Outcomes)
	}
}

// learn.settings may name any tracked JSON layer a generator merges into
// the live settings file, not just one named settings.json.
func TestGraduateIntoLayeredSettings(t *testing.T) {
	f := newApplyFixture(t, enforcedProposals)
	ctx := context.Background()
	settings := filepath.Join(f.dot, ".claude", "settings.base.json")
	writeFile(t, settings, "{\n  \"model\": \"opus\"\n}\n")
	gitIn(t, f.dot, "add", "-A")
	gitIn(t, f.dot, "commit", "-q", "-m", "settings layer")
	gitIn(t, f.dot, "push", "-q", "origin", "HEAD:main")
	f.applier.Settings = settings

	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Routes[0].Skip != "" || plan.Routes[1].Skip != "" {
		t.Fatalf("skips: %q, %q", plan.Routes[0].Skip, plan.Routes[1].Skip)
	}
	scripts := fakeApply("#!/bin/sh")
	f.editor.Apply = func(input string, dirs []string) (string, error) {
		out, err := scripts(input, dirs)
		claude := filepath.Join(dirs[0], ".claude", "CLAUDE.md")
		if werr := os.WriteFile(claude, []byte("# Rules\n"), 0o644); werr != nil {
			return "", werr
		}
		return out + "\nApplied[graduate]: Ask before a push -- " + claude, err
	}
	if _, err := f.applier.Run(ctx, plan); err != nil {
		t.Fatal(err)
	}
	got := gitIn(t, f.dot, "show", plan.Branch+":.claude/settings.base.json")
	for _, want := range []string{`"model": "opus"`, `"ask": [`, `"Bash(git push:*)"`, `"matcher": "Bash"`} {
		if !strings.Contains(got, want) {
			t.Errorf("layer on the branch misses %s:\n%s", want, got)
		}
	}
	if live, _ := os.ReadFile(settings); string(live) != "{\n  \"model\": \"opus\"\n}\n" {
		t.Errorf("the live layer file changed: %s", live)
	}
}

// A git-ignored learn.settings file is likely generated, so a graduate into
// it is held back with a reason that names the key.
func TestGraduateHoldsIgnoredSettings(t *testing.T) {
	f := newApplyFixture(t, enforcedProposals)
	ctx := context.Background()
	writeFile(t, filepath.Join(f.dot, ".gitignore"), ".claude/settings.json\n")
	gitIn(t, f.dot, "add", "-A")
	gitIn(t, f.dot, "commit", "-q", "-m", "ignore the generated settings")
	gitIn(t, f.dot, "push", "-q", "origin", "HEAD:main")
	settings := filepath.Join(f.dot, ".claude", "settings.json")
	writeFile(t, settings, "{}\n")
	f.applier.Settings = settings

	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Routes[:2] {
		if !strings.Contains(r.Skip, "learn.settings") || !strings.Contains(r.Skip, "git-ignored") {
			t.Errorf("%q skip = %q, want it to name learn.settings and the ignore", r.Title, r.Skip)
		}
	}
}

// A graduate held back by Run's pre-check on the staged settings copy
// reports an Outcome that carries its Route id, like every other skip, so
// the outcome can be traced back to the plan's route.
func TestRunSettingsPrecheckOutcomeCarriesRoute(t *testing.T) {
	f := newApplyFixture(t, enforcedProposals)
	ctx := context.Background()
	settings := filepath.Join(f.dot, ".claude", "settings.json")
	// The committed copy, which Run stages, is a JSON array the binary
	// can't add to; the live copy is an object, so the plan passes.
	writeFile(t, settings, "[]\n")
	gitIn(t, f.dot, "add", "-A")
	gitIn(t, f.dot, "commit", "-q", "-m", "settings")
	gitIn(t, f.dot, "push", "-q", "origin", "HEAD:main")
	writeFile(t, settings, "{}\n")
	f.applier.Settings = settings

	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Routes[0].Skip != "" {
		t.Fatalf("planned skip = %q, want the hold to arise inside Run", plan.Routes[0].Skip)
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range res.Outcomes {
		if !strings.Contains(o.Detail, "can't add it to the settings file") {
			continue
		}
		found = true
		if o.Route == "" {
			t.Errorf("the pre-check outcome has no route: %+v", o)
		}
	}
	if !found {
		t.Errorf("no outcome reports the settings hold: %+v", res.Outcomes)
	}
}

// A graduate of a rule that lives in a repo's own context file writes that
// repo's .claude/settings.json, through the same binary-side merge, with no
// learn.settings set, and only when all cited evidence is from that repo.
func TestRepoGraduateWritesRepoSettings(t *testing.T) {
	ctx := context.Background()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	project := liveRepo(t, base, "project", map[string]string{
		"CLAUDE.md": "# Project\n\n- Ask before a push.\n- Never force-push.\n",
	})
	kit := liveRepo(t, base, "kit", map[string]string{"bin/README.md": "scripts\n"})
	repo, err := DiscoverRepo(project)
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(base, "state", "2026-11-11")
	writeFile(t, filepath.Join(run, "proposals.md"), "## Proposals\n\n"+
		"### graduate: Ask before a push\n- replaces: r-0101\n- enforce: permission\n- permission: ask Bash(git push:*)\n- rule: none\n- evidence: c-0001\n\n"+
		"### graduate: Block force pushes\n- target: bin/no-force-push\n- replaces: r-0102\n- enforce: hook\n- hook: PreToolUse Bash\n- rule: a script sketch\n- evidence: c-0001\n\n"+
		"### graduate: Earned elsewhere\n- replaces: r-0101\n- enforce: permission\n- permission: deny Bash(git push --force:*)\n- rule: none\n- evidence: c-0002\n")
	evidence := RunEvidence{
		"c-0001": {{Session: "s1", Day: "2026-11-01", Repo: "~/project", Owner: true}},
		"c-0002": {{Session: "s2", Day: "2026-11-02", Repo: "~/elsewhere", Owner: true}},
	}
	if err := evidence.save(run); err != nil {
		t.Fatal(err)
	}
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	pub := &review.Fake{}
	claude := filepath.Join(project, "CLAUDE.md")
	editor := &model.FakeEditor{Apply: func(input string, dirs []string) (string, error) {
		at := func(live string) string {
			rel, _ := filepath.Rel(project, live)
			return filepath.Join(dirs[0], rel)
		}
		if err := os.WriteFile(at(claude), []byte("# Project\n"), 0o644); err != nil {
			return "", err
		}
		script := filepath.Join(dirs[1], "bin", "no-force-push")
		writeFile(t, script, "#!/bin/sh\nexit 2\n")
		return "Applied[graduate]: Ask before a push -- " + at(claude) + "\n" +
			"Applied[graduate]: Block force pushes -- " + script, nil
	}}
	a := &Applier{
		Editor: editor, Model: "sonnet", Publisher: pub,
		Repos: []Repo{repo}, Home: base, Clock: fakeClock, RequireOwnerEvidence: true,
		Bin: filepath.Join(kit, "bin"), Skills: filepath.Join(kit, "skills"),
		Ledger: &Ledger{Rules: []Rule{
			{ID: "r-0101", Target: claude, Text: "Ask before a push."},
			{ID: "r-0102", Target: claude, Text: "Never force-push."},
		}},
	}
	plan, err := a.Plan(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(project, ".claude", "settings.json")
	skips := map[string]string{}
	edits := map[string]string{}
	for _, r := range plan.Routes {
		skips[r.Title] = r.Skip
		edits[r.Title] = strings.Join(r.Edit, ",")
		if r.Skip == "" && r.Settings != settings {
			t.Errorf("%q settings = %q, want the repo's own %s", r.Title, r.Settings, settings)
		}
	}
	for title, want := range map[string]string{
		"Ask before a push":  "",
		"Block force pushes": "",
		"Earned elsewhere":   "which ran in ~/elsewhere",
	} {
		if (want == "") != (skips[title] == "") || !strings.Contains(skips[title], want) {
			t.Errorf("%q: skip = %q, want %q", title, skips[title], want)
		}
	}
	if edits["Ask before a push"] != claude+","+settings {
		t.Errorf("edits = %q, want the repo's context file and its settings", edits["Ask before a push"])
	}

	// The bin checkout holds the hook's script too, so the plan spans both.
	if len(plan.Repos) != 2 || plan.Repos[0] != project {
		t.Fatalf("plan repos = %v", plan.Repos)
	}
	res, err := a.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, o := range res.Outcomes {
		if o.Applied {
			applied++
		}
	}
	if applied != 2 {
		t.Errorf("outcomes = %+v", res.Outcomes)
	}
	got := gitIn(t, project, "show", plan.Branch+":.claude/settings.json")
	for _, want := range []string{`"ask": [`, `"Bash(git push:*)"`, `"matcher": "Bash"`, `"command": "\"$HOME\"/kit/bin/no-force-push"`} {
		if !strings.Contains(got, want) {
			t.Errorf("repo settings on the branch miss %s:\n%s", want, got)
		}
	}
	if prose := gitIn(t, project, "show", plan.Branch+":CLAUDE.md"); strings.Contains(prose, "Ask before a push") {
		t.Errorf("the prose rule survived:\n%s", prose)
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Error("the live repo settings file was written before review")
	}
}
