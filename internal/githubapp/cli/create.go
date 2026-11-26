package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/micahhausler/recall/internal/githubapp/app"
	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/config"
	"github.com/micahhausler/recall/internal/githubapp/keys"
	"github.com/micahhausler/recall/internal/githubapp/manifest"
)

// noConfigYet refuses to set up a machine that already has an App: the
// config would be replaced, orphaning the key it points at.
func (e *env) noConfigYet() error {
	_, err := config.Load(e.configPath)
	if errors.Is(err, config.ErrNone) {
		return nil
	}
	// A config that can't be read is still a config.
	return fmt.Errorf("%s already exists, so this machine already has an App; `status` shows it, and `import-key` adds or rotates a key", e.tilde(e.configPath))
}

// create registers the App through GitHub's manifest flow, stores its
// private key, and writes the config. The key goes from GitHub's answer
// into the key store from memory; it is never written in the clear unless
// the key kind is file. GitHub hands the key over once, so the moment it
// arrives it is kept and the config written, before anything that could
// fail for another reason.
// manifestText is what the App's GitHub page shows.
type manifestText struct{ name, description, homepage string }

func (e *env) create(ctx context.Context, m manifestText, keyRef string, port int) error {
	name := m.name
	ref, err := keys.Parse(keyRef, e.home)
	if err != nil {
		return err
	}
	store, ok := ref.Store()
	if !ok {
		return fmt.Errorf("create can't put a new key into %s; create with a file: or tpm: key, or add a key from the App's settings page to the agent and run import-key", ref.Kind)
	}
	if err := e.noConfigYet(); err != nil {
		return err
	}
	// The config must have somewhere to go too, before GitHub is asked.
	if err := config.Ready(e.configPath); err != nil {
		return err
	}
	cfg := &config.Config{}
	flow := &manifest.Flow{
		Manifest: func(redirect string) manifest.Manifest {
			return manifest.New(m.name, m.description, m.homepage, redirect, tokenPermissions)
		},
		Port: port, Web: e.web, API: e.api, Client: e.client, Out: e.stdout,
		Listening: e.listening,
		Ready:     store.Ready,
		Store: func(conv *manifest.Conversion) error {
			key, err := appkey.Parse([]byte(conv.PEM))
			if err != nil {
				return err
			}
			cfg.ClientID, cfg.AppID, cfg.Slug, cfg.Owner = conv.ClientID, conv.ID, conv.Slug, conv.Owner.Login
			return e.keep(cfg, keyRef, ref, key)
		},
	}
	conv, err := flow.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "\ncreated %s (%s), owned by %s, client ID %s\n", conv.Slug, conv.HTMLURL, conv.Owner.Login, conv.ClientID)
	if conv.Name != name {
		fmt.Fprintf(e.stdout, "WARNING: GitHub named it %q, not %q. If you didn't rename it on GitHub's page, this isn't the App you asked for: delete %s and the key.\n", conv.Name, name, e.tilde(e.configPath))
	}
	fmt.Fprintf(e.stdout, "stored its key at %s:%s, and signed with it\n", ref.Kind, e.tilde(ref.Path))
	fmt.Fprintf(e.stdout, "wrote %s\n", e.tilde(e.configPath))
	// The bot user's ID is a convenience in the config; token asks GitHub
	// for it when it's missing, so a failed lookup here loses nothing.
	if err := e.recordBot(ctx, cfg, ref); err != nil {
		fmt.Fprintf(e.stdout, "WARNING: the bot user's ID isn't in the config yet (%v); `token` asks GitHub for it each run until it is.\n", err)
	}
	fmt.Fprintf(e.stdout, "\nNext:\n  1. Install it on only the repositories recall opens pull requests on:\n     %s/apps/%s/installations/new\n", e.web, conv.Slug)
	fmt.Fprintf(e.stdout, "  2. Check it: %s status\n", e.name)
	fmt.Fprintf(e.stdout, "  3. Point recall at it:\n     [review.credentials]\n     command = [%q, \"token\"]\n", e.name)
	return nil
}

// recordBot adds the bot user's ID to a config that is already written.
func (e *env) recordBot(ctx context.Context, cfg *config.Config, ref keys.Ref) error {
	signer, err := ref.Open(cfg.KeyFingerprint)
	if err != nil {
		return err
	}
	a := &app.App{ClientID: cfg.ClientID, Key: signer, API: e.api, HTTP: e.client, Now: e.now}
	bot, err := a.Bot(ctx, "")
	if err != nil {
		return err
	}
	cfg.BotUserID = bot.ID
	return cfg.Save(e.configPath)
}
