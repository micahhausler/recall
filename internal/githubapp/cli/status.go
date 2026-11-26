package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/githubapp/app"
	"github.com/micahhausler/recall/internal/githubapp/appkey"
)

// statusTokenCap bounds how many repositories status mints a test token
// for, so an installation on every repository doesn't mint hundreds.
const statusTokenCap = 20

// status shows what GitHub says about the App and proves the key works by
// minting a token for each repository it is installed on. It prints no
// token and revokes each one it mints. A problem is reported in place and
// makes the command fail.
func (e *env) status(ctx context.Context) error {
	cfg, ref, a, err := e.open()
	if err != nil {
		return err
	}
	fp, err := appkey.Fingerprint(a.Key.Public())
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "config:     %s\n", e.tilde(e.configPath))
	fmt.Fprintf(e.stdout, "key:        %s:%s  %s\n", ref.Kind, e.tilde(ref.Path), fp)
	info, err := a.Info(ctx)
	if err != nil {
		return fmt.Errorf("authenticating as the App: %w", err)
	}
	fmt.Fprintf(e.stdout, "app:        %s (%s), id %d, owned by %s\n", info.Slug, info.HTMLURL, info.ID, info.Owner.Login)
	fmt.Fprintf(e.stdout, "client ID:  %s\n", cfg.ClientID)
	fmt.Fprintf(e.stdout, "may:        %s\n", permissionList(info.Permissions))

	var problems []string
	problem := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if len(info.Events) > 0 {
		problem("the App subscribes to events (%s); nothing listens, so turn its webhook off", strings.Join(info.Events, ", "))
	}
	if err := person(info.Owner); err != nil {
		problem("%v", err)
	}
	for name, level := range tokenPermissions {
		if !app.Grants(info.Permissions[name], level) {
			problem("the App lacks %s: %s; add it on its settings page, then accept the change on each installation", name, level)
		}
	}
	// A token is asked for less than the App holds, but whoever holds the
	// key can ask for all of it.
	for name, level := range info.Permissions {
		if name == "metadata" && level == "read" {
			continue // every App holds it
		}
		want, used := tokenPermissions[name]
		if !used || (level != want && app.Grants(level, want)) {
			problem("the App holds %s: %s, more than a token ever uses; remove it on its settings page", name, level)
		}
	}
	// What the config caches about the App must match GitHub.
	bot, err := a.Bot(ctx, "")
	if err != nil {
		problem("looking up the bot user: %v", err)
	} else {
		botName, botEmail := bot.CommitIdentity()
		fmt.Fprintf(e.stdout, "commits as: %s <%s>\n", botName, botEmail)
		if cfg.BotUserID != 0 && cfg.BotUserID != bot.ID {
			problem("config bot_user_id is %d; GitHub says %d", cfg.BotUserID, bot.ID)
		}
	}
	for _, c := range []struct {
		key       string
		have, got string
	}{
		{"slug", cfg.Slug, info.Slug},
		{"owner", cfg.Owner, info.Owner.Login},
		{"app_id", idString(cfg.AppID), idString(info.ID)},
	} {
		if c.have != "" && c.have != c.got {
			problem("config %s is %s; GitHub says %s", c.key, c.have, c.got)
		}
	}

	installs, err := a.Installations(ctx)
	if err != nil {
		problem("listing where the App is installed: %v", err)
	} else if len(installs) == 0 {
		problem("the App isn't installed anywhere; install it at %s/apps/%s/installations/new", e.web, info.Slug)
	}
	minted := 0
	for _, in := range installs {
		repos, err := a.Repositories(ctx, in.ID)
		if err != nil {
			problem("listing %s's repositories: %v", in.Account.Login, err)
			continue
		}
		fmt.Fprintf(e.stdout, "installed:  on %s (%s repositories)\n", in.Account.Login, in.RepositorySelection)
		if in.RepositorySelection != "selected" {
			problem("the installation on %s covers %s repositories; choose only the ones recall opens pull requests on, at %s/settings/installations/%d", in.Account.Login, in.RepositorySelection, e.web, in.ID)
		}
		for _, full := range repos {
			if minted >= statusTokenCap {
				fmt.Fprintf(e.stdout, "  %s: not tested (only the first %d are)\n", full, statusTokenCap)
				continue
			}
			minted++
			owner, name, _ := strings.Cut(full, "/")
			t, err := a.Token(ctx, owner, name, tokenPermissions)
			if err != nil {
				problem("minting a token for %s: %v", full, err)
				continue
			}
			// The token proved what it was minted to prove. It could
			// write to the repository for an hour, so it ends here.
			if err := a.Revoke(ctx, t); err != nil {
				problem("the test token for %s is still live until %s: %v", full, t.Expires.UTC().Format("15:04Z"), err)
				continue
			}
			fmt.Fprintf(e.stdout, "  %s: token minted, then revoked\n", full)
		}
	}
	for _, p := range problems {
		fmt.Fprintf(e.stdout, "PROBLEM:    %s\n", p)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s) found", len(problems))
	}
	return nil
}

func idString(id int64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprint(id)
}

func permissionList(perms map[string]string) string {
	var list []string
	for name, level := range perms {
		list = append(list, name+": "+level)
	}
	sort.Strings(list)
	return strings.Join(list, ", ")
}
