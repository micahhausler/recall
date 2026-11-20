package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
)

func init() {
	Register("fs", func(ctx context.Context, s config.StoreSettings) (Store, error) {
		return NewFS(s.Path)
	})
}

// FS stores objects as files under a root directory.
type FS struct {
	root string
}

// NewFS returns a store rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("fs store: store.path is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FS{root: dir}, nil
}

func (f *FS) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", fmt.Errorf("bad key %q", key)
	}
	return filepath.Join(f.root, filepath.FromSlash(key)), nil
}

// PutIfAbsent claims the key with O_EXCL, writes a temp file, and renames it
// over the claim, so a partial write never looks like an object to another
// reader and a second writer gets ErrExists.
func (f *FS) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	path, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for {
		claim, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			claim.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if !deadClaim(path) {
			return fmt.Errorf("%s: %w", key, ErrExists)
		}
		// A writer died between claiming the key and filling it. The empty
		// file is no object, and left alone it would read as missing and
		// refuse every later write, forever. Clear it and race the
		// exclusive create again, so exactly one taker wins the key.
		if err := takeOver(path); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*")
	if err != nil {
		os.Remove(path)
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(path)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(path)
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// Put replaces the object at key: a temp file renamed over the old one, so
// a reader never sees a partial write.
func (f *FS) Put(ctx context.Context, key string, r io.Reader) error {
	path, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// claimGrace is how long an empty claim is left to the writer that made it.
const claimGrace = time.Minute

// deadClaim reports whether path is an empty claim nobody is filling.
func deadClaim(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() == 0 && time.Since(st.ModTime()) > claimGrace
}

// takeOver clears the dead claim at path so the exclusive create can be
// retried. The claim is renamed aside before removal, and a rename has
// exactly one winner, so two takers can never both think they cleared the
// key, and a claim or object a racer wrote after the deadness check is
// detected and put back rather than deleted.
func takeOver(path string) error {
	grave, err := os.CreateTemp(filepath.Dir(path), ".put-*")
	if err != nil {
		return err
	}
	grave.Close()
	defer os.Remove(grave.Name())
	err = os.Rename(path, grave.Name())
	if errors.Is(err, fs.ErrNotExist) {
		// Another taker cleared the claim first; retry the create.
		return nil
	}
	if err != nil {
		return err
	}
	if !deadClaim(grave.Name()) {
		// Between the deadness check and the rename, a racer filled the key
		// or claimed it afresh. Put it back.
		return os.Rename(grave.Name(), path)
	}
	return nil
}

func (f *FS) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	path, err := f.path(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	return file, err
}

func (f *FS) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(f.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(f.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) && !strings.HasPrefix(filepath.Base(key), ".put-") {
			keys = append(keys, key)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

// Delete removes the object at key; a missing key is no error, matching S3.
func (f *FS) Delete(ctx context.Context, key string) error {
	path, err := f.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (f *FS) Head(ctx context.Context, key string) (Info, error) {
	path, err := f.path(key)
	if err != nil {
		return Info{}, err
	}
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: st.Size()}, nil
}
