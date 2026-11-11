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
	claim, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", key, ErrExists)
		}
		return err
	}
	claim.Close()
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
