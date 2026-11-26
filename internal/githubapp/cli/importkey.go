package cli

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/micahhausler/recall/internal/githubapp/app"
	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/config"
	"github.com/micahhausler/recall/internal/githubapp/keys"
)

// keep puts key where ref says, proves the kept key signs, and writes the
// config, in that order. It makes no network call: by now GitHub has
// accepted this key or just issued it, so nothing here can fail for a
// reason unrelated to keeping it. Whatever the caller means to destroy
// afterward is still there when keep fails.
func (e *env) keep(cfg *config.Config, keyRef string, ref keys.Ref, key *rsa.PrivateKey) error {
	fp, err := appkey.Fingerprint(&key.PublicKey)
	if err != nil {
		return err
	}
	if store, ok := ref.Store(); ok {
		if err := store.Put(key); err != nil {
			return err
		}
	}
	// For an ssh-agent, nothing was stored: opening finds the key the
	// agent must already hold, by its fingerprint.
	signer, err := ref.Open(fp)
	if err != nil {
		return fmt.Errorf("opening the key at %s: %w", ref, err)
	}
	// The same signature a JWT takes, checked against the key's public
	// half: the key that was kept is this key, and it signs.
	msg := []byte(e.name + " proves a kept key signs")
	sig, err := crypto.SignMessage(signer, rand.Reader, msg, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("the key at %s doesn't sign: %w", ref, err)
	}
	digest := sha256.Sum256(msg)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("the key at %s signs, but not as the key it was stored from: %w", ref, err)
	}
	cfg.Key, cfg.KeyFingerprint = keyRef, fp
	if err := cfg.Save(e.configPath); err != nil {
		return fmt.Errorf("the key is at %s, but writing %s failed: %w", ref, e.tilde(e.configPath), err)
	}
	return nil
}

// person refuses an App an organization owns: the owner is who recall
// asks to review, and whose review comments count.
func person(owner app.Account) error {
	if owner.Type != "User" {
		return fmt.Errorf("the App is owned by %s, which is a %s; this tool is for one person's App", owner.Login, owner.Type)
	}
	return nil
}

// importKey stores a private key downloaded from the App's settings page,
// then points the config at it. Everything that asks GitHub happens first,
// with the downloaded key; then the key is kept and the config written;
// and only then is the downloaded file destroyed.
func (e *env) importKey(ctx context.Context, keyRef, clientID, src string) error {
	ref, err := keys.Parse(keyRef, e.home)
	if err != nil {
		return err
	}
	// An existing config names the App; a new machine needs --client-id.
	cfg := &config.Config{}
	if existing, err := config.Load(e.configPath); err == nil {
		cfg = existing
	} else if !errors.Is(err, config.ErrNone) {
		return err // the config is there but unreadable
	}
	if clientID != "" && cfg.ClientID != "" && clientID != cfg.ClientID {
		return fmt.Errorf("--client-id %s isn't the App in %s (%s)", clientID, e.tilde(e.configPath), cfg.ClientID)
	}
	if cfg.ClientID == "" {
		cfg.ClientID = clientID
	}
	if cfg.ClientID == "" {
		return errors.New("which App is the key for? Pass --client-id, from the App's settings page")
	}
	store, stores := ref.Store()
	if stores {
		if err := store.Ready(); err != nil {
			return err
		}
	}
	if err := config.Ready(e.configPath); err != nil {
		return err
	}
	var data []byte
	if src == "-" {
		data, err = io.ReadAll(io.LimitReader(e.stdin, 1<<20))
	} else {
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return err
	}
	key, err := appkey.Parse(data)
	if err != nil {
		return fmt.Errorf("reading the private key: %w", err)
	}
	// Ask GitHub whether this key is the App's, and for what the config
	// records about the App, while nothing has changed yet.
	probe := &app.App{ClientID: cfg.ClientID, Key: key, API: e.api, HTTP: e.client, Now: e.now}
	info, err := probe.Info(ctx)
	if err != nil {
		return fmt.Errorf("GitHub doesn't accept this key for the App %s: %w", cfg.ClientID, err)
	}
	if err := person(info.Owner); err != nil {
		return err
	}
	cfg.AppID, cfg.Slug, cfg.Owner = info.ID, info.Slug, info.Owner.Login
	bot, err := probe.Bot(ctx, "")
	if err != nil {
		return fmt.Errorf("looking up the App's bot user: %w", err)
	}
	cfg.BotUserID = bot.ID

	old, oldFP := cfg.Key, cfg.KeyFingerprint
	if err := e.keep(cfg, keyRef, ref, key); err != nil {
		return err
	}
	fp := cfg.KeyFingerprint
	fmt.Fprintf(e.stdout, "wrote %s\n", e.tilde(e.configPath))
	switch {
	case !stores:
		// An agent may hold keys only until it restarts, so the download
		// may be the only lasting copy; destroying it is the owner's call.
		fmt.Fprintf(e.stdout, "the agent at %s holds the key, and signed with it\n  %s (the same fingerprint the App's settings page lists)\n", e.tilde(ref.Path), fp)
		if src != "-" {
			fmt.Fprintf(e.stdout, "WARNING: %s still holds the key in the clear. Delete it once the agent keeps the key for good.\n", src)
		}
	default:
		fmt.Fprintf(e.stdout, "stored the key at %s:%s, and signed with it\n  %s (the same fingerprint the App's settings page lists)\n", ref.Kind, e.tilde(ref.Path), fp)
		if src == "-" {
			break
		}
		if err := shred(src); err != nil {
			fmt.Fprintf(e.stdout, "WARNING: %s still holds the key in the clear, and destroying it failed: %v\n", src, err)
		} else {
			fmt.Fprintf(e.stdout, "destroyed %s (overwritten, then removed; on an SSD or a copy-on-write filesystem the old blocks may outlast that)\n", src)
		}
	}
	switch {
	case old != "" && old != keyRef:
		fmt.Fprintf(e.stdout, "The config pointed at %s before; once `status` passes, delete that key, and its entry on the App's settings page.\n", old)
	case oldFP != "" && oldFP != fp:
		fmt.Fprintf(e.stdout, "The config named the key %s before; once `status` passes, delete that key, and its entry on the App's settings page.\n", oldFP)
	}
	return nil
}

// shred overwrites a file with random bytes and removes it: the most a
// program can do to a plaintext key someone downloaded.
func shred(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, rand.Reader, st.Size()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}
