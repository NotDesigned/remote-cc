package session

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var profileRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validProfile(p string) bool {
	return profileRe.MatchString(p) && p != "." && p != ".."
}

// MachineProfile returns this A machine's default history profile:
// "<hostname>-<random>", generated once and stored in the user config dir,
// so two machines never collide even if they share a hostname.
func MachineProfile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	f := filepath.Join(dir, "rcc", "profile")
	if b, err := os.ReadFile(f); err == nil {
		if p := strings.TrimSpace(string(b)); validProfile(p) {
			return p, nil
		}
	}
	p := sanitizeHost() + "-" + randHex(4)
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(f, []byte(p+"\n"), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func sanitizeHost() string {
	h, _ := os.Hostname()
	h = strings.ToLower(h)
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	h = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(h, "-")
	h = strings.Trim(h, "-")
	if len(h) > 40 {
		h = h[:40]
	}
	if h == "" {
		h = "host"
	}
	return h
}
