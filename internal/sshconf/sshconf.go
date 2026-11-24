// Package sshconf manages this machine's side of remote-cc's ssh setup:
// per-server keys and config entries in ~/.ssh/remote-cc/, the Include line
// in ~/.ssh/config, pinned host keys in ~/.ssh/known_hosts, and invites.
//
// For a server reached as alias A there are two entries: "A" (the user's
// container, root@rcc-<user>) and "A-gw" (the gateway account <user>), the
// first using the second as ProxyJump.
package sshconf

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

var (
	aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,40}$`)
	userRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

// ValidAlias reports whether a is usable as an ssh alias.
func ValidAlias(a string) bool { return aliasRe.MatchString(a) }

// ValidUser checks a server user name.
func ValidUser(n string) error {
	if !userRe.MatchString(n) {
		return fmt.Errorf("invalid user name %q: lowercase letters, digits and '-', starting with a letter, at most 31 characters", n)
	}
	return nil
}

// DefaultAlias is "<first label>-cc" ("server-la.example.com" ->
// "server-la-cc"), so it does not shadow an existing alias for the host
// itself (our file is included at the top of ~/.ssh/config, so it would win).
func DefaultAlias(host string) string {
	h := strings.Trim(host, "[]")
	if strings.Count(h, ":") > 1 || regexp.MustCompile(`^[0-9.]+$`).MatchString(h) {
		return "cc"
	}
	a := strings.ToLower(strings.SplitN(h, ".", 2)[0])
	if !aliasRe.MatchString(a) {
		return "cc"
	}
	return a + "-cc"
}

// Server describes how a configured alias reaches its container.
type Server struct {
	Alias, Host, User string
	Port              int
	Jump              string // ProxyJump for the gateway, if the port is not reachable directly
}

func (s Server) gwKnownName() string {
	if s.Port == 22 {
		return s.Host
	}
	return fmt.Sprintf("[%s]:%d", s.Host, s.Port)
}

func (s Server) containerKnownName() string { return fmt.Sprintf("rcc-%s@%s", s.User, s.Host) }

func dirs() (sshDir, rccDir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	sshDir = filepath.Join(home, ".ssh")
	rccDir = filepath.Join(sshDir, "remote-cc")
	return sshDir, rccDir, os.MkdirAll(rccDir, 0o700)
}

// KeyPath is this machine's private key for alias.
func KeyPath(alias string) (string, error) {
	_, d, err := dirs()
	return filepath.Join(d, alias+"_ed25519"), err
}

// EnsureKey creates this machine's ed25519 key for alias unless it exists and
// returns its public key line.
func EnsureKey(alias string) (string, error) {
	path, err := KeyPath(alias)
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(path + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	// The comment names this machine in "device list": user, host, alias.
	host, _ := os.Hostname()
	who := ""
	if u, err := user.Current(); err == nil {
		who = u.Username
		if i := strings.LastIndexAny(who, `\`); i >= 0 {
			who = who[i+1:] // Windows: DOMAIN\name
		}
	}
	clean := func(s string) string {
		return strings.Trim(regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(strings.ToLower(s), "-"), "-")
	}
	comment := "remote-cc-" + alias
	if h := clean(host); h != "" {
		comment = h + "-" + alias
		if w := clean(who); w != "" {
			comment = w + "@" + comment
		}
	}
	priv, line, err := NewKey(comment)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return "", err
	}
	return line, os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
}

// NewKey makes an ed25519 key: OpenSSH private key PEM and public key line.
func NewKey(comment string) (priv []byte, pubLine string, err error) {
	pub, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return nil, "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
	if comment != "" {
		line += " " + comment
	}
	return pem.EncodeToMemory(block), line, nil
}

// Fingerprint formats an authorized_keys line or "type base64" like
// ssh-keygen -l ("SHA256:…"); "" if it is not a key.
func Fingerprint(line string) string {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		f := strings.Fields(line)
		if len(f) < 2 {
			return ""
		}
		blob, err := base64.StdEncoding.DecodeString(f[1])
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(blob)
		return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	}
	return ssh.FingerprintSHA256(pk)
}

// Configure pins both host keys and writes the alias entries for s.
func Configure(s Server, gwKey, containerKey string) error {
	sshDir, d, err := dirs()
	if err != nil {
		return err
	}
	for _, l := range []string{s.gwKnownName() + " " + gwKey, s.containerKnownName() + " " + containerKey} {
		if strings.Count(l, " ") < 2 {
			return errors.New("missing host keys")
		}
		if err := appendLine(filepath.Join(sshDir, "known_hosts"), l); err != nil {
			return err
		}
	}
	gwHost, jump := s.Host, ""
	if s.Jump != "" {
		gwHost, jump = "127.0.0.1", "    ProxyJump "+s.Jump+"\n"
	}
	block := fmt.Sprintf(`# remote-cc:%[1]s begin
Host %[1]s-gw
    HostName %[2]s
    Port %[3]d
    User %[4]s
%[7]s    IdentityFile ~/.ssh/remote-cc/%[1]s_ed25519
    IdentitiesOnly yes
    HostKeyAlias %[5]s
Host %[1]s
    HostName rcc-%[4]s
    Port 22
    User root
    ProxyJump %[1]s-gw
    HostKeyAlias %[6]s
    IdentityFile ~/.ssh/remote-cc/%[1]s_ed25519
    IdentitiesOnly yes
    ServerAliveInterval 15
# remote-cc:%[1]s end
`, s.Alias, gwHost, s.Port, s.User, s.gwKnownName(), s.containerKnownName(), jump)
	if err := upsertBlock(filepath.Join(d, "config"), s.Alias, block); err != nil {
		return err
	}
	return ensureInclude(filepath.Join(sshDir, "config"))
}

// Lookup reads back the Server written by Configure for alias.
func Lookup(alias string) (Server, error) {
	_, d, err := dirs()
	if err != nil {
		return Server{}, err
	}
	b, _ := os.ReadFile(filepath.Join(d, "config"))
	m := blockRe(alias).Find(b)
	if m == nil {
		return Server{}, fmt.Errorf("%q is not a remote-cc server on this machine (see ~/.ssh/remote-cc/config)", alias)
	}
	s := Server{Alias: alias}
	gw := true
	for _, l := range strings.Split(string(m), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "Host":
			gw = strings.HasSuffix(f[1], "-gw")
		case gw && f[0] == "User":
			s.User = f[1]
		case gw && f[0] == "ProxyJump":
			s.Jump = f[1]
		case gw && f[0] == "HostKeyAlias": // "[host]:port" or "host"
			h := f[1]
			s.Port = 22
			if strings.HasPrefix(h, "[") {
				if i := strings.LastIndex(h, "]:"); i > 0 {
					s.Port, _ = strconv.Atoi(h[i+2:])
					h = h[1:i]
				}
			}
			s.Host = h
		}
	}
	if s.Host == "" || s.User == "" {
		return Server{}, fmt.Errorf("cannot read the entry for %q in ~/.ssh/remote-cc/config", alias)
	}
	return s, nil
}

// Verify checks that `ssh <alias> hostname` reaches rcc-<user>.
func Verify(alias, user string) error {
	out, err := exec.Command("ssh", "-o", "BatchMode=yes", alias, "hostname").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "rcc-"+user {
		return fmt.Errorf("`ssh %s hostname` did not answer rcc-%s:\n%s", alias, user, strings.TrimSpace(string(out)))
	}
	return nil
}

func blockRe(alias string) *regexp.Regexp {
	q := regexp.QuoteMeta(alias)
	return regexp.MustCompile(`(?ms)^# remote-cc:` + q + ` begin\n.*?^# remote-cc:` + q + ` end\n`)
}

func appendLine(path, line string) error {
	if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(line)) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func upsertBlock(path, alias, block string) error {
	old, _ := os.ReadFile(path)
	re := blockRe(alias)
	var s string
	if re.Match(old) {
		s = re.ReplaceAllLiteralString(string(old), block)
	} else {
		s = string(old)
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += block
	}
	return os.WriteFile(path, []byte(s), 0o600)
}

// ensureInclude puts "Include remote-cc/config" first in ~/.ssh/config
// (before any Host block, so it applies globally).
func ensureInclude(path string) error {
	old, _ := os.ReadFile(path)
	if regexp.MustCompile(`(?mi)^\s*Include\s+.*remote-cc[/\\]config`).Match(old) {
		return nil
	}
	return os.WriteFile(path, append([]byte("Include remote-cc/config\n\n"), old...), 0o600)
}
