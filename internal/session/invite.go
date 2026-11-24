package session

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"remote-cc/deploy"
)

// An invite lets a user enroll their own key without sending it to the
// admin. It carries a one-time key that is authorized for the user's account
// with `restrict,port-forwarding,command="rcc-enroll <id>",expiry-time=…`:
// enough to hop through the gateway into the user's container, where it can
// only run rcc-enroll, which authorizes the user's new key and deletes the
// invite key. The invite also pins both host keys, so `join` never has to
// trust a host key on first use.
type invite struct {
	V       int    `json:"v"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Alias   string `json:"alias"`
	GwKey   string `json:"gw"`
	UserKey string `json:"uk"`
	ID      string `json:"id"`
	Key     string `json:"key"` // one-time private key (OpenSSH PEM)
	Expires string `json:"exp"`
}

const invitePrefix = "rcc1."

func (iv *invite) encode() string {
	b, _ := json.Marshal(iv)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeInvite(s string) (*invite, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, invitePrefix) {
		return nil, errors.New("not a remote-cc invite (it should start with rcc1.)")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, invitePrefix))
	if err != nil {
		return nil, fmt.Errorf("damaged invite: %w", err)
	}
	var iv invite
	if err := json.Unmarshal(b, &iv); err != nil || iv.V != 1 || iv.Host == "" || iv.Key == "" {
		return nil, errors.New("damaged or unsupported invite")
	}
	if err := validUser(iv.User); err != nil {
		return nil, err
	}
	return &iv, nil
}

const userInviteScript = multiEnv + `
[ -f "$DIR/users/$NAME/.env" ] || { echo "rcc: no user $NAME (create it with 'user add')" >&2; exit 3; }
k="$DATA/keys/$NAME/authorized_keys"; inv="$DATA/keys/$NAME.invites"
touch "$inv"; chmod 644 "$inv"
# Containers made by older rcc mounted the key file read-only: refresh them.
printf '%s' "$COMPOSE_B64" | base64 -d > "$DIR/users/$NAME/compose.yml"
(cd "$DIR/users/$NAME" && docker compose up -d --quiet-pull >&2)
exp=$(date -d "+$TTL hours" +%Y%m%d%H%M)
# Drop expired invite lines while we are here.
now=$(date +%Y%m%d%H%M)
for f in "$k" "$inv"; do
  awk -v now="$now" '{ if (match($0, /expiry-time="[0-9]+"/)) { t=substr($0, RSTART+13, 12); if (t < now) next } print }' "$f" > "$f.tmp" && cat "$f.tmp" > "$f" && rm -f "$f.tmp"
done
# Gateway half: may only hop to this user's container.
printf 'restrict,port-forwarding,permitopen="rcc-%s:22",expiry-time="%s" %s rcc-invite-%s\n' "$NAME" "$exp" "$PUB" "$ID" >> "$inv"
# Container half: may only run rcc-enroll; no forwarding of any kind.
printf 'restrict,command="/usr/local/bin/rcc-enroll %s",expiry-time="%s" %s rcc-invite-%s\n' "$ID" "$exp" "$PUB" "$ID" >> "$k"
echo "exp=$exp"
echo "ukey=$(cat "$DATA/users/$NAME/ssh/ssh_host_ed25519_key.pub" 2>/dev/null | awk '{print $1" "$2}')"
`

// UserInvite creates a one-time invite for an existing user.
func UserInvite(mc MultiConfig, name, alias string, ttl time.Duration) error {
	if err := validUser(name); err != nil {
		return err
	}
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	return userInvite(s, mc, name, alias, ttl)
}

func userInvite(s *session, mc MultiConfig, name, alias string, ttl time.Duration) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	id := randHex(8)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "rcc-invite-"+id)
	if err != nil {
		return err
	}
	hours := int(ttl.Hours())
	if hours < 1 {
		hours = 1
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	out, err := s.runElevated(vars("DIR", mc.InstallDir, "NAME", name, "ID", id, "PUB", pubLine,
		"TTL", strconv.Itoa(hours), "COMPOSE_B64", base64Std(deploy.ComposeUser)), userInviteScript)
	if err != nil {
		return fmt.Errorf("invite failed: %w", err)
	}
	f := parseFacts(out)
	host := mc.publicHost()
	port, _ := strconv.Atoi(f["port"])
	if alias == "" {
		alias = defaultAlias(host)
	}
	iv := invite{V: 1, Host: host, Port: port, User: name, Alias: alias, GwKey: f["gwkey"], UserKey: f["ukey"],
		ID: id, Key: string(pem.EncodeToMemory(block)), Expires: f["exp"]}
	fmt.Fprintf(os.Stderr, "\nOne-time invite for %s on %s (valid until %s server time). Send it privately;\n"+
		"whoever holds it can enroll one key for %s until it is used or expires.\n\n", name, host, f["exp"], name)
	fmt.Println(iv.encode())
	fmt.Fprintf(os.Stderr, "\nOn their machine (after installing remote-cc):\n  remote-cc join <invite>\n")
	return nil
}

// defaultAlias is "<first label>-cc" ("server-la.example" -> "server-la-cc"),
// so it does not shadow an existing ssh alias for the host itself (our config
// is included at the top of ~/.ssh/config, so it would win).
func defaultAlias(host string) string {
	if net := strings.Trim(host, "[]"); strings.Count(net, ":") > 1 || regexp.MustCompile(`^[0-9.]+$`).MatchString(net) {
		return "cc"
	}
	a := strings.ToLower(strings.SplitN(host, ".", 2)[0])
	if !aliasRe.MatchString(a) {
		return "cc"
	}
	return a + "-cc"
}

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,40}$`)

// Join enrolls this machine with an invite: new key, pinned host keys, ssh
// config entries "<alias>" (the container) and "<alias>-gw" (the gateway).
func Join(inviteText, alias string) error {
	iv, err := decodeInvite(inviteText)
	if err != nil {
		return err
	}
	if alias == "" {
		alias = iv.Alias
	}
	if alias == "" {
		alias = "cc"
	}
	if !aliasRe.MatchString(alias) {
		return fmt.Errorf("invalid alias %q", alias)
	}
	if iv.Port == 0 {
		iv.Port = 2222
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	dir := filepath.Join(sshDir, "remote-cc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// 1. This machine's own key for this server.
	keyPath := filepath.Join(dir, alias+"_ed25519")
	pubLine, err := ensureKey(keyPath, alias)
	if err != nil {
		return err
	}

	// 2. Pin the host keys from the invite.
	gwName := iv.Host
	if iv.Port != 22 {
		gwName = fmt.Sprintf("[%s]:%d", iv.Host, iv.Port)
	}
	ukAlias := fmt.Sprintf("rcc-%s@%s", iv.User, iv.Host)
	khPath := filepath.Join(sshDir, "known_hosts")
	for _, l := range []string{gwName + " " + iv.GwKey, ukAlias + " " + iv.UserKey} {
		if strings.Count(l, " ") < 2 {
			return errors.New("invite lacks host keys")
		}
		if err := appendLine(khPath, l); err != nil {
			return err
		}
	}

	// 3. Enroll the new key with the one-time key, through a private config.
	tmpKey := filepath.Join(dir, ".invite-"+iv.ID)
	if err := os.WriteFile(tmpKey, []byte(iv.Key), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmpKey)
	tmpCfg := tmpKey + ".conf"
	cfg := fmt.Sprintf(`Host gw
    HostName %s
    Port %d
    User %s
    IdentityFile "%s"
    IdentitiesOnly yes
    UserKnownHostsFile "%s"
    StrictHostKeyChecking yes
    HostKeyAlias %s
Host target
    HostName rcc-%s
    Port 22
    User root
    ProxyJump gw
    IdentityFile "%s"
    IdentitiesOnly yes
    UserKnownHostsFile "%s"
    StrictHostKeyChecking yes
    HostKeyAlias %s
`, iv.Host, iv.Port, iv.User, slash(tmpKey), slash(khPath), gwName, iv.User, slash(tmpKey), slash(khPath), ukAlias)
	if err := os.WriteFile(tmpCfg, []byte(cfg), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmpCfg)
	fmt.Fprintf(os.Stderr, "remote-cc: enrolling this machine as %s on %s …\n", iv.User, iv.Host)
	c := exec.Command("ssh", "-F", tmpCfg, "-o", "BatchMode=yes", "target")
	c.Stdin = strings.NewReader(pubLine + "\n")
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil || !strings.Contains(out.String(), "enrolled") {
		return fmt.Errorf("enrollment failed (the invite may be used, expired or revoked):\n%s", strings.TrimSpace(out.String()))
	}

	// 4. ssh config: our own file, included from ~/.ssh/config.
	block := fmt.Sprintf(`# remote-cc:%[1]s begin
Host %[1]s-gw
    HostName %[2]s
    Port %[3]d
    User %[4]s
    IdentityFile ~/.ssh/remote-cc/%[1]s_ed25519
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
`, alias, iv.Host, iv.Port, iv.User, gwName, ukAlias)
	if err := upsertBlock(filepath.Join(dir, "config"), alias, block); err != nil {
		return err
	}
	if err := ensureInclude(filepath.Join(sshDir, "config")); err != nil {
		return err
	}

	// 5. Check.
	chk, err := exec.Command("ssh", "-o", "BatchMode=yes", alias, "hostname").CombinedOutput()
	if err != nil || strings.TrimSpace(string(chk)) != "rcc-"+iv.User {
		return fmt.Errorf("enrolled, but `ssh %s hostname` did not answer rcc-%s:\n%s", alias, iv.User, strings.TrimSpace(string(chk)))
	}
	fmt.Printf("\nDone: this machine can now reach %s as \"%s\".\n  %s login %s     # sign in to Claude (once)\n  %s %s           # in a project directory\n",
		iv.Host, alias, Prog, alias, Prog, alias)
	return nil
}

// ensureKey creates an ed25519 key at path unless it exists; returns the
// public key line.
func ensureKey(path, alias string) (string, error) {
	if b, err := os.ReadFile(path + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	comment := "remote-cc-" + regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(strings.ToLower(host), "-") + "-" + alias
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".pub", []byte(line+"\n"), 0o644); err != nil {
		return "", err
	}
	return line, nil
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

// upsertBlock replaces the "# remote-cc:<alias> begin … end" block in path.
func upsertBlock(path, alias, block string) error {
	old, _ := os.ReadFile(path)
	re := regexp.MustCompile(`(?ms)^# remote-cc:` + regexp.QuoteMeta(alias) + ` begin\n.*?^# remote-cc:` + regexp.QuoteMeta(alias) + ` end\n`)
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

// ensureInclude puts "Include remote-cc/config" at the top of ~/.ssh/config
// (before any Host block, so it applies globally).
func ensureInclude(path string) error {
	old, _ := os.ReadFile(path)
	if regexp.MustCompile(`(?mi)^\s*Include\s+.*remote-cc[/\\]config`).Match(old) {
		return nil
	}
	return os.WriteFile(path, append([]byte("Include remote-cc/config\n\n"), old...), 0o600)
}

func slash(p string) string { return filepath.ToSlash(p) }
