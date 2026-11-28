package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// AnalysisPrefix holds what observe writes: observations, rule hits,
// memory notes, and evidence.
const AnalysisPrefix = "analysis/"

// reviewsPrefix holds the reducer's per-host reviews, which skip the disk.
const reviewsPrefix = "analysis/reviews/"

// Pending keeps analysis/ keys on local disk first: writes land in a local
// directory that mirrors the store's keys, reads check it before the
// remote, and List merges both. Upload copies what the remote lacks. Every
// other key, analysis/reviews/ included, goes straight to the remote.
//
// A remote call under analysis/ that fails reads as missing, so an
// unreachable store doesn't stop a pass; after the first failure, which
// RemoteErr reports, the remote isn't asked again. An empty local file is
// a write in flight or a dead claim, never an object.
type Pending struct {
	local  *FS
	remote Store
	marks  string

	mu        sync.Mutex
	remoteErr error
}

// NewPending opens dir as the local copy of remote's analysis/ keys, with
// marks holding a marker per key already uploaded. Both are created 0700.
func NewPending(dir, marks string, remote Store) (*Pending, error) {
	for _, d := range []string{dir, marks} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Pending{local: &FS{root: dir, private: true}, remote: remote, marks: marks}, nil
}

func pendingKey(key string) bool {
	return strings.HasPrefix(key, AnalysisPrefix) && !strings.HasPrefix(key, reviewsPrefix)
}

// RemoteErr is the first remote error a read or write passed over.
func (p *Pending) RemoteErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.remoteErr
}

func (p *Pending) down() bool { return p.RemoteErr() != nil }

// fault passes over a remote error, except the context's own, which it
// returns.
func (p *Pending) fault(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteErr == nil {
		p.remoteErr = err
	}
	return nil
}

func missing(key string) error { return fmt.Errorf("%s: %w", key, ErrNotExist) }

// localHead is the local object's info; an empty file is missing.
func (p *Pending) localHead(ctx context.Context, key string) (Info, error) {
	info, err := p.local.Head(ctx, key)
	if err == nil && info.Size == 0 {
		return Info{}, missing(key)
	}
	return info, err
}

func (p *Pending) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	if !pendingKey(key) {
		return p.remote.PutIfAbsent(ctx, key, r)
	}
	if _, err := p.localHead(ctx, key); err == nil {
		return fmt.Errorf("%s: %w", key, ErrExists)
	} else if !errors.Is(err, ErrNotExist) {
		return err
	}
	if !p.down() {
		switch _, err := p.remote.Head(ctx, key); {
		case err == nil:
			return fmt.Errorf("%s: %w", key, ErrExists)
		case !errors.Is(err, ErrNotExist):
			if err := p.fault(ctx, err); err != nil {
				return err
			}
		}
	}
	// FS's own claim takes over a dead one.
	return p.local.PutIfAbsent(ctx, key, r)
}

// Put on an analysis/ key has no caller; it replaces the local copy and
// marks it waiting again.
func (p *Pending) Put(ctx context.Context, key string, r io.Reader) error {
	if !pendingKey(key) {
		return p.remote.Put(ctx, key, r)
	}
	if err := p.local.Put(ctx, key, r); err != nil {
		return err
	}
	return p.unmark(key)
}

func (p *Pending) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !pendingKey(key) {
		return p.remote.Get(ctx, key)
	}
	if _, err := p.localHead(ctx, key); err == nil {
		return p.local.Get(ctx, key)
	} else if !errors.Is(err, ErrNotExist) {
		return nil, err
	}
	if p.down() {
		return nil, missing(key)
	}
	rc, err := p.remote.Get(ctx, key)
	if err != nil && !errors.Is(err, ErrNotExist) {
		if err := p.fault(ctx, err); err != nil {
			return nil, err
		}
		return nil, missing(key)
	}
	return rc, err
}

func (p *Pending) Head(ctx context.Context, key string) (Info, error) {
	if !pendingKey(key) {
		return p.remote.Head(ctx, key)
	}
	info, err := p.localHead(ctx, key)
	if !errors.Is(err, ErrNotExist) {
		return info, err
	}
	if p.down() {
		return Info{}, missing(key)
	}
	info, err = p.remote.Head(ctx, key)
	if err != nil && !errors.Is(err, ErrNotExist) {
		if err := p.fault(ctx, err); err != nil {
			return Info{}, err
		}
		return Info{}, missing(key)
	}
	return info, err
}

func (p *Pending) List(ctx context.Context, prefix string) ([]string, error) {
	if !strings.HasPrefix(prefix, AnalysisPrefix) && !strings.HasPrefix(AnalysisPrefix, prefix) || strings.HasPrefix(prefix, reviewsPrefix) {
		return p.remote.List(ctx, prefix)
	}
	var remote []string
	if !pendingKey(prefix) || !p.down() {
		var err error
		remote, err = p.remote.List(ctx, prefix)
		if err != nil {
			if !pendingKey(prefix) {
				return nil, err
			}
			if err := p.fault(ctx, err); err != nil {
				return nil, err
			}
		}
	}
	local, err := p.local.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(remote)+len(local))
	var keys []string
	for _, k := range remote {
		seen[k] = true
		keys = append(keys, k)
	}
	for _, k := range local {
		if !seen[k] {
			if _, err := p.localHead(ctx, k); err != nil {
				continue // an empty claim
			}
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete on an analysis/ key has no caller; it removes the local copy, its
// marker, and the remote object.
func (p *Pending) Delete(ctx context.Context, key string) error {
	if pendingKey(key) {
		if err := p.local.Delete(ctx, key); err != nil {
			return err
		}
		if err := p.unmark(key); err != nil {
			return err
		}
	}
	return p.remote.Delete(ctx, key)
}

func (p *Pending) mark(key string) error {
	path := filepath.Join(p.marks, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}

func (p *Pending) unmark(key string) error {
	err := os.Remove(filepath.Join(p.marks, filepath.FromSlash(key)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Upload copies each local key the remote lacks and marks it, so a later
// upload and Waiting skip it. A key the remote already holds counts as
// done; when its body differs, warn names the key. Empty files are writes
// in flight and wait. It returns how many objects it wrote.
func (p *Pending) Upload(ctx context.Context, warn io.Writer) (copied int, err error) {
	keys, err := waitingKeys(p.local.root, p.marks)
	if err != nil {
		return 0, err
	}
	for _, key := range keys {
		data, err := readAll(ctx, p.local, key)
		if err != nil {
			return copied, err
		}
		if len(data) == 0 {
			continue
		}
		err = p.remote.PutIfAbsent(ctx, key, bytes.NewReader(data))
		switch {
		case err == nil:
			copied++
		case errors.Is(err, ErrExists):
			if theirs, err := readAll(ctx, p.remote, key); err != nil || !bytes.Equal(theirs, data) {
				fmt.Fprintf(warn, "recall: warning: the store already holds %s with other content; kept the store's copy\n", key)
			}
		default:
			return copied, err
		}
		if err := p.mark(key); err != nil {
			return copied, err
		}
	}
	return copied, nil
}

func readAll(ctx context.Context, s Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Waiting counts the local objects not yet uploaded.
func (p *Pending) Waiting() (int, error) { return Waiting(p.local.root, p.marks) }

// Waiting counts the objects under dir with no marker under marks; a
// missing dir holds none, and an empty file is no object.
func Waiting(dir, marks string) (int, error) {
	keys, err := waitingKeys(dir, marks)
	return len(keys), err
}

func waitingKeys(dir, marks string) ([]string, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	keys, err := (&FS{root: dir}).List(context.Background(), "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, key := range keys {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
		if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Size() == 0 {
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(marks, filepath.FromSlash(key))); errors.Is(err, fs.ErrNotExist) {
			out = append(out, key)
		} else if err != nil {
			return nil, err
		}
	}
	return out, nil
}
