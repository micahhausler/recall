// Package store is where archives and later analysis live. A Store holds
// immutable objects under slash-separated keys; backends are registered by
// the name used in config ("fs", "s3"), so adding one is a new file, not a
// change to callers.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/micahhausler/recall/internal/config"
)

// ErrExists is returned by PutIfAbsent when the key already holds an object.
var ErrExists = errors.New("object already exists")

// ErrNotExist is returned by Get and Head for a missing key.
var ErrNotExist = errors.New("object does not exist")

// Info describes a stored object.
type Info struct {
	Size int64
}

// Store holds immutable objects: an upload never overwrites.
type Store interface {
	// PutIfAbsent writes an object only if the key is empty; ErrExists otherwise.
	PutIfAbsent(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns every key under the prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	Head(ctx context.Context, key string) (Info, error)
}

// Factory opens a backend from the merged store settings.
type Factory func(ctx context.Context, s config.StoreSettings) (Store, error)

var registry = map[string]Factory{}

// Register adds a backend under its config name.
func Register(kind string, f Factory) {
	registry[kind] = f
}

// Open opens the backend settings.Kind names.
func Open(ctx context.Context, s config.StoreSettings) (Store, error) {
	f, ok := registry[s.Kind]
	if !ok {
		kinds := make([]string, 0, len(registry))
		for k := range registry {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		return nil, fmt.Errorf("unknown store.kind %q; have %s", s.Kind, strings.Join(kinds, ", "))
	}
	return f(ctx, s)
}

// Mem is an in-memory Store for tests.
type Mem struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{objects: map[string][]byte{}}
}

func (m *Mem) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok {
		return fmt.Errorf("%s: %w", key, ErrExists)
	}
	m.objects[key] = data
	return nil
}

func (m *Mem) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (m *Mem) List(ctx context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *Mem) Head(ctx context.Context, key string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return Info{}, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	return Info{Size: int64(len(data))}, nil
}
