// Package atomicfile replaces small config files safely: under a lock (so
// two rcc processes never lose each other's change in a read-modify-write)
// and by renaming a complete, synced new file over the old one (so a crash
// or a reader never sees half a file).
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Write replaces path with data. A symlink at path is followed (the file it
// points to is replaced, e.g. a ~/.ssh/config kept in a dotfiles repo), and
// an existing file keeps its mode; a new one gets perm.
func Write(path string, data []byte, perm fs.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	// Windows has no unix modes (Chmod there only sets read-only).
	if err := f.Chmod(perm); err != nil && !isWindows {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// LockTimeout is how long Lock waits for another rcc to finish.
var LockTimeout = 15 * time.Second

// Lock takes an exclusive lock for path (on path+".lock", which stays) and
// returns its release. The lock goes away with the process, so a crash
// leaves none behind.
func Lock(path string) (func(), error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(LockTimeout)
	for {
		got, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if got {
			return func() { unlock(f); f.Close() }, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%s is locked by another remote-cc; try again", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Update replaces path with what fn makes of its current content (nil if
// it does not exist), under the lock. fn returning an error changes nothing.
func Update(path string, perm fs.FileMode, fn func(old []byte) ([]byte, error)) error {
	unlock, err := Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b, err := fn(old)
	if err != nil {
		return err
	}
	return Write(path, b, perm)
}

// KeepDamaged copies a file that could not be parsed next to it (as
// <name>.damaged-<time>) and returns that path, so that nothing is lost
// when it is replaced.
func KeepDamaged(path string, b []byte) (string, error) {
	p := path + ".damaged-" + time.Now().Format("20060102-150405")
	return p, os.WriteFile(p, b, 0o600)
}
