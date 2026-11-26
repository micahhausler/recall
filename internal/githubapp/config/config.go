// Package config reads and writes this machine's settings: which GitHub
// App it is and where its private key lives. Nothing in the file is a
// secret; the key it points at is, unless only this machine's TPM can use
// it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is one machine's settings.
type Config struct {
	// ClientID is the App's client ID, the issuer of its JWTs.
	ClientID string `toml:"client_id"`
	// AppID and Slug identify the App on GitHub.
	AppID int64  `toml:"app_id,omitempty"`
	Slug  string `toml:"slug,omitempty"`
	// BotUserID is the ID of the App's bot user, <slug>[bot], which its
	// commit email carries.
	BotUserID int64 `toml:"bot_user_id,omitempty"`
	// Owner is the login of the account that owns the App: the person
	// whose proposals these are.
	Owner string `toml:"owner,omitempty"`
	// Key is where this machine holds the App's private key, as
	// <kind>:<path>.
	Key string `toml:"key"`
	// KeyFingerprint is the key's fingerprint as the App's settings page
	// lists it. It picks the key out of an SSH agent, and for any kind
	// the key found must match it.
	KeyFingerprint string `toml:"key_fingerprint,omitempty"`
	// ExtraPermissions are what the App holds on purpose beyond what
	// recall's tokens use, as name to read or write; `token --permission`
	// may ask for them.
	ExtraPermissions map[string]string `toml:"extra_permissions,omitempty"`
}

// Path is the config file's place under home, for the command name.
func Path(home, name string) string {
	return filepath.Join(home, ".config", name, "config.toml")
}

// ErrNone reports that there is no config file.
var ErrNone = errors.New("no config file")

// Load reads the config at path. A missing file is ErrNone, with how to
// make one; a file without a client ID or key is an error too.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s doesn't exist (%w); `create` registers the App and writes it, and `import-key` writes it for an App that already exists", path, ErrNone)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	// A key this version doesn't know is an error: a misspelled
	// key_fingerprint would otherwise turn its check off without a word.
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var unknown *toml.StrictMissingError
		if errors.As(err, &unknown) {
			return nil, fmt.Errorf("%s has a key this version doesn't know:\n%s", path, unknown.String())
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.ClientID == "" {
		return nil, fmt.Errorf("%s: client_id isn't set", path)
	}
	if c.Key == "" {
		return nil, fmt.Errorf("%s: key isn't set", path)
	}
	for name, level := range c.ExtraPermissions {
		if level != "read" && level != "write" {
			return nil, fmt.Errorf("%s: extra_permissions.%s is %q; it is read or write", path, name, level)
		}
	}
	return &c, nil
}

// Ready reports why a config couldn't be written at path, making its
// directory when it is missing. A command about to keep a key it can't
// get twice asks first.
func Ready(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("the config can't be written in %s: %w", dir, err)
	}
	probe.Close()
	return os.Remove(probe.Name())
}

// Save writes the config to path, readable by its owner only, replacing
// the file in one step.
func (c *Config) Save(path string) error {
	data, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
