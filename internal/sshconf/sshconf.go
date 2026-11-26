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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"remote-cc/internal/atomicfile"
	"remote-cc/internal/remote"
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
	if err := s.validate(); err != nil {
		return err
	}
	gwKey, err := HostKey(gwKey)
	if err != nil {
		return fmt.Errorf("the gateway's host key: %w", err)
	}
	containerKey, err = HostKey(containerKey)
	if err != nil {
		return fmt.Errorf("the container's host key: %w", err)
	}
	sshDir, d, err := dirs()
	if err != nil {
		return err
	}
	kh := filepath.Join(sshDir, "known_hosts")
	if err := pinHostKey(kh, s.gwKnownName(), gwKey); err != nil {
		return err
	}
	if err := pinHostKey(kh, s.containerKnownName(), containerKey); err != nil {
		return err
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
	stopMasters(s.Alias)
	return ensureInclude(filepath.Join(sshDir, "config"))
}

// Aliases lists the HOSTs configured on this machine (server setup and
// device join write them to ~/.ssh/remote-cc/config), in file order,
// without their gateways.
func Aliases() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".ssh", "remote-cc", "config"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.EqualFold(f[0], "Host") && !strings.HasSuffix(f[1], "-gw") && !strings.ContainsAny(f[1], "*?") {
			out = append(out, f[1])
		}
	}
	return out
}

// Hosts reads back every configured HOST. An entry that cannot be read
// (edited by hand) comes back with only its Alias.
func Hosts() []Server {
	var out []Server
	for _, a := range Aliases() {
		s, err := Lookup(a)
		if err != nil {
			s = Server{Alias: a}
		}
		out = append(out, s)
	}
	return out
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

// pinHostKey makes key the only trusted host key for name in the
// known_hosts file at path: earlier entries for exactly that name (plain or
// hashed) are dropped, so a server's old key is not trusted after a new one
// was pinned. Entries naming other hosts too, and @-marker lines, are kept.
func pinHostKey(path, name, key string) error {
	return atomicfile.Update(path, 0o600, func(old []byte) ([]byte, error) {
		b := dropKnownHosts(old, name)
		return append(b, name+" "+key+"\n"...), nil
	})
}

// forgetHostKeys drops the known_hosts entries for exactly these names.
func forgetHostKeys(path string, names ...string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return atomicfile.Update(path, 0o600, func(old []byte) ([]byte, error) {
		return dropKnownHosts(old, names...), nil
	})
}

func dropKnownHosts(old []byte, names ...string) []byte {
	var out bytes.Buffer
	for _, l := range strings.SplitAfter(string(old), "\n") {
		if l == "" {
			continue
		}
		if f := strings.Fields(l); len(f) >= 2 && slices.ContainsFunc(names, func(n string) bool { return knownHostIs(f[0], n) }) {
			continue
		}
		out.WriteString(l)
		if !strings.HasSuffix(l, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// knownHostIs reports whether a known_hosts host field names exactly name,
// as written or hashed (|1|salt|hash, see ssh-keygen -H).
func knownHostIs(field, name string) bool {
	if field == name {
		return true
	}
	p := strings.Split(field, "|")
	if len(p) != 4 || p[0] != "" || p[1] != "1" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(p[2])
	sum, err2 := base64.StdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(name))
	return hmac.Equal(m.Sum(nil), sum)
}

func upsertBlock(path, alias, block string) error {
	return atomicfile.Update(path, 0o600, func(old []byte) ([]byte, error) {
		re := blockRe(alias)
		if re.Match(old) {
			return []byte(re.ReplaceAllLiteralString(string(old), block)), nil
		}
		s := string(old)
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		return []byte(s + block), nil
	})
}

// removeBlock drops alias's entries from the config file at path.
func removeBlock(path, alias string) error {
	return atomicfile.Update(path, 0o600, func(old []byte) ([]byte, error) {
		return blockRe(alias).ReplaceAll(old, nil), nil
	})
}

var includeRe = regexp.MustCompile(`(?mi)^\s*Include\s+.*remote-cc[/\\]config`)

// ensureInclude puts "Include remote-cc/config" first in ~/.ssh/config
// (before any Host block, so it applies globally).
func ensureInclude(path string) error {
	if old, err := os.ReadFile(path); err == nil && includeRe.Match(old) {
		return nil
	}
	return atomicfile.Update(path, 0o600, func(old []byte) ([]byte, error) {
		if includeRe.Match(old) {
			return old, nil
		}
		return append([]byte("Include remote-cc/config\n\n"), old...), nil
	})
}

// Forget removes what Configure wrote for every HOST on this machine that
// reaches the server at host:port (its config entries and both pinned host
// keys) and returns their aliases. For `server remove`.
func Forget(host string, port int) ([]string, error) {
	sshDir, d, err := dirs()
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, s := range Hosts() {
		if s.Host != host || s.Port != port {
			continue
		}
		if err := removeBlock(filepath.Join(d, "config"), s.Alias); err != nil {
			return gone, err
		}
		if err := forgetHostKeys(filepath.Join(sshDir, "known_hosts"), s.containerKnownName(), s.gwKnownName()); err != nil {
			return gone, err
		}
		stopMasters(s.Alias)
		gone = append(gone, s.Alias)
	}
	return gone, nil
}

// stopMasters ends shared ssh connections (see internal/remote) to alias
// and its gateway: one made before the alias was re-configured or a key
// removed would go on using the old settings and the old authentication.
func stopMasters(alias string) {
	remote.StopMaster(remote.Target{Dest: alias})
	remote.StopMaster(remote.Target{Dest: alias + "-gw"})
}
