package cli

import (
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/githubapp/app/apptest"
	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/config"
)

func TestStatusShowsTheAppAndMintsATestToken(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	if code := f.run("", "status"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, f.stderr, f.stdout)
	}
	out := f.stdout.String()
	fp, _ := appkey.Fingerprint(&testKey().PublicKey)
	for _, want := range []string{
		"key:        file:~/app.pem  " + fp,
		"app:        recall-you (https://github.com/apps/recall-you), id 7, owned by you",
		"commits as: recall-you[bot] <4242+recall-you[bot]@users.noreply.github.com>",
		"installed:  on you (selected repositories)",
		"you/dotfiles: token minted, then revoked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ghs_") {
		t.Errorf("status printed a token:\n%s", out)
	}
	// One token listed the repositories and one proved the mint; neither
	// outlives the command.
	if minted, live := len(f.github.Requests), f.github.Live(); minted != 2 || len(live) != 0 {
		t.Errorf("status minted %d tokens and left %d live, want 2 and 0: %+v", minted, len(live), live)
	}
}

// A test token that couldn't be revoked can still write for an hour, so
// status says so and fails.
func TestStatusReportsATokenItCouldNotRevoke(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	f.github.NoRevoke = true
	code := f.run("", "status")
	out := f.stdout.String()
	if code != 1 || !strings.Contains(out, "PROBLEM:    listing you's repositories: revoking a token: github DELETE /installation/token: 500") {
		t.Errorf("exit %d, want 1 and the failed revoke named:\n%s", code, out)
	}
	if strings.Contains(out, "ghs_") {
		t.Errorf("status printed a token:\n%s", out)
	}
}

// What the config caches must match GitHub, and the App must hold what a
// token asks for; each mismatch is named and fails the command.
func TestStatusReportsProblems(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-old", BotUserID: 1, Owner: "you"})
	delete(f.github.Granted, "checks")
	code := f.run("", "status")
	out := f.stdout.String()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		"PROBLEM:    config slug is recall-old; GitHub says recall-you",
		"PROBLEM:    config bot_user_id is 1; GitHub says 4242",
		"PROBLEM:    the App lacks checks: read",
		"PROBLEM:    minting a token for you/dotfiles",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	f = newFixture(t)
	f.configured(config.Config{})
	f.github.Installed = map[string]int64{}
	if code := f.run("", "status"); code != 1 || !strings.Contains(f.stdout.String(), "isn't installed anywhere; install it at https://github.example/apps/recall-you/installations/new") {
		t.Errorf("not installed: exit %d:\n%s", code, f.stdout)
	}
}

// What the key's holder could do beyond what a token asks for is a
// problem too: an App owned by an organization, an installation on every
// repository, and permissions no token uses.
func TestStatusReportsMoreReachThanATokenUses(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	f.github.OwnerType = "Organization"
	f.github.Selection = "all"
	f.github.Granted["administration"] = "write"
	f.github.Granted["checks"] = "write"
	code := f.run("", "status")
	out := f.stdout.String()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		"PROBLEM:    the App is owned by you, which is a Organization",
		"PROBLEM:    the installation on you covers all repositories",
		"PROBLEM:    the App holds administration: write, more than a token ever uses",
		"PROBLEM:    the App holds checks: write, more than a token ever uses",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the App holds contents") || strings.Contains(out, "the App holds metadata") {
		t.Errorf("status complains about a permission a token uses:\n%s", out)
	}
}

// A revoke GitHub may have done fails the second time, so it isn't asked
// again.
func TestStatusAsksForEachRevokeOnce(t *testing.T) {
	f := retryFixture(t)
	f.github.Faults = map[string][]apptest.Fault{revokeCall: {{Status: 500}, {Status: 500}, {Status: 500}}}
	if code := f.run("", "status"); code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, f.stdout)
	}
	if calls, minted := f.github.CallCount(revokeCall), len(f.github.Requests); calls != minted || minted == 0 {
		t.Errorf("%d revokes for %d tokens; want one each", calls, minted)
	}
	if strings.Contains(f.stdout.String(), "ghs_") {
		t.Errorf("status printed a token:\n%s", f.stdout)
	}
}

// A permission the config's extra_permissions name is expected: held, it
// is no problem, and missing, it is one.
func TestStatusExpectsTheConfigsExtraPermissions(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you",
		ExtraPermissions: map[string]string{"issues": "write"}})
	if code := f.run("", "status"); code != 1 || !strings.Contains(f.stdout.String(), "the App lacks issues: write") {
		t.Errorf("exit %d, want 1 for the missing issues: write:\n%s", code, f.stdout)
	}
	f.github.Granted["issues"] = "write"
	if code := f.run("", "status"); code != 0 {
		t.Errorf("exit %d, want 0 with issues: write held:\n%s", code, f.stdout)
	}
}
