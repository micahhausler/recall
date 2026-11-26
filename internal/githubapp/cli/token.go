package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/api/credentials/v1/command"
	"github.com/micahhausler/recall/internal/githubapp/app"
)

// The contract with recall is recall's own: credentialsv1 is its object,
// and command.Run is the command's side of it. Run reads the request from
// RECALL_EXEC_INFO, refuses a terminal, and writes the answer; what is left
// here is where the token comes from.

// token answers one credential request with a token that reaches only the
// requested repository, with what every token uses plus the asked
// permissions the config's extra_permissions hold.
func (e *env) token(ctx context.Context, asked map[string]string) error {
	err := command.Run(ctx, e.execInfo, e.stdout, func(ctx context.Context, req command.Request) (credentialsv1.Status, error) {
		none := credentialsv1.Status{}
		if req.Host != "github.com" {
			return none, fmt.Errorf("spec.repo %s is on %s; the App works on github.com only", req.Spec.Repo, req.Host)
		}
		cfg, _, a, err := e.open()
		if err != nil {
			return none, err
		}
		permissions := maps.Clone(tokenPermissions)
		held := expectedPermissions(cfg)
		for name, level := range asked {
			if !app.Grants(held[name], level) {
				return none, fmt.Errorf("--permission %s=%s: the config's extra_permissions don't hold it; add it there and to the App", name, level)
			}
			permissions[name] = level
		}
		t, err := a.Token(ctx, req.Owner, req.Name, permissions)
		if errors.Is(err, app.ErrNotInstalled) {
			return none, fmt.Errorf("%w; %s", err, e.installHint(cfg.Slug))
		}
		if err != nil {
			return none, err
		}
		// A config written by hand may lack what GitHub can tell us; ask,
		// but don't write: this command only reads.
		if err := describe(ctx, a, cfg, t.Value); err != nil {
			return none, err
		}
		bot := app.User{ID: cfg.BotUserID, Login: cfg.Slug + "[bot]"}
		botName, botEmail := bot.CommitIdentity()
		return credentialsv1.Status{
			Token: t.Value, ExpirationTimestamp: &t.Expires,
			CommitIdentity: &credentialsv1.CommitIdentity{Name: botName, Email: botEmail},
			Owner:          cfg.Owner,
		}, nil
	})
	if errors.Is(err, command.ErrTerminal) {
		return fmt.Errorf("%w, or run `status` to check the App without showing one", err)
	}
	return err
}

// installHint says where to install the App.
func (e *env) installHint(slug string) string {
	if slug == "" {
		return "install it on the repository from the App's settings page"
	}
	return fmt.Sprintf("install it at %s/apps/%s/installations/new", e.web, slug)
}
