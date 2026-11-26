// Package keyfile writes a key to a new file. A key file may be the only
// copy of a key, so nothing here ever replaces one.
package keyfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// vacant reports an error when something is already at path.
func vacant(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists and may be the only copy of a key; move it away or name another path with --key", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Ready reports why a key can't be written at path: something is there, or
// its directory can't be made or written to. It makes the directory and
// proves a file can be created in it, since the caller is about to accept
// a key it will be handed exactly once.
func Ready(path string) error {
	if err := vacant(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return fmt.Errorf("a key can't be written in %s: %w", dir, err)
	}
	probe.Close()
	return os.Remove(probe.Name())
}

// Create writes data to a new file at path, readable by its owner only,
// making the directory when it is missing. It fails when path exists. The
// file and its directory entry are flushed to disk before it returns, so
// a caller may then destroy the copy the data came from.
func Create(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
