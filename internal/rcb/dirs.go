package rcb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"remote-cc/internal/atomicfile"
	"remote-cc/internal/sshconf"
)

// Use is what `rcb use` remembers for a project directory.
type Use struct {
	Profile string `json:"profile"` // profile id
	Server  string `json:"server"`  // ssh alias of the remote-cc server
}

func dirsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rcc", "rcb-dirs.json"), nil
}

func loadDirs() (map[string]Use, error) {
	p, err := dirsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Use{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseDirs(p, b)
}

// parseDirs reads rcb-dirs.json. A damaged file is an error, never an
// empty list that the next Remember would write over.
func parseDirs(p string, b []byte) (map[string]Use, error) {
	m := map[string]Use{}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is damaged (%v); fix or delete it", p, err)
	}
	return m, nil
}

// Remember stores u for dir (an absolute path). A damaged file is kept as a
// copy (named in the error) and not replaced.
func Remember(dir string, u Use) error {
	p, err := dirsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return atomicfile.Update(p, 0o600, func(old []byte) ([]byte, error) {
		m, err := parseDirs(p, old)
		if err != nil {
			if c, cerr := atomicfile.KeepDamaged(p, old); cerr == nil {
				err = fmt.Errorf("%w (a copy is in %s)", err, c)
			}
			return nil, err
		}
		m[filepath.Clean(dir)] = u
		b, _ := json.MarshalIndent(m, "", "  ")
		return append(b, '\n'), nil
	})
}

// Lookup finds what was remembered for dir or its nearest parent, and that directory.
func Lookup(dir string) (Use, string, bool) {
	m, err := loadDirs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcc: warning: %v\n", err)
		return Use{}, "", false
	}
	if len(m) == 0 {
		return Use{}, "", false
	}
	d := filepath.Clean(dir)
	for {
		if u, ok := m[d]; ok && u.Profile != "" {
			return u, d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return Use{}, "", false
		}
		d = parent
	}
}

// Servers lists the HOSTs this machine has (see sshconf.Aliases).
func Servers() []string { return sshconf.Aliases() }
