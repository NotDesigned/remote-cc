package rcb

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

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
	m := map[string]Use{}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(b, &m)
	return m, nil
}

// Remember stores u for dir (an absolute path).
func Remember(dir string, u Use) error {
	m, err := loadDirs()
	if err != nil {
		return err
	}
	m[filepath.Clean(dir)] = u
	p, _ := dirsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Lookup finds what was remembered for dir or its nearest parent, and that directory.
func Lookup(dir string) (Use, string, bool) {
	m, err := loadDirs()
	if err != nil || len(m) == 0 {
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
