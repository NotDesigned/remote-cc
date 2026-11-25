package sshconf

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Invite lets a machine enroll its own key for a user without sending it to
// anyone. It carries a one-time key, authorized for the user in two halves
// with expiry-time: at the gateway (may only hop to rcc-<user>:22) and in the
// user's container (may only run rcc-enroll, which authorizes one plain key
// and deletes the one-time key). It also pins both host keys, so joining
// never trusts a host key on first use.
type Invite struct {
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

func (iv *Invite) Encode() string {
	b, _ := json.Marshal(iv)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func DecodeInvite(s string) (*Invite, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, invitePrefix) {
		return nil, errors.New("not a remote-cc invite (it should start with rcc1.)")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, invitePrefix))
	if err != nil {
		return nil, fmt.Errorf("damaged invite: %w", err)
	}
	var iv Invite
	if err := json.Unmarshal(b, &iv); err != nil || iv.V != 1 || iv.Host == "" || iv.Key == "" {
		return nil, errors.New("damaged or unsupported invite")
	}
	if err := ValidUser(iv.User); err != nil {
		return nil, err
	}
	if iv.Port == 0 {
		iv.Port = 2222
	}
	return &iv, nil
}

// Join enrolls this machine with an invite: its own key, pinned host keys,
// the alias entries, and a final check.
func Join(text, alias string) (Server, error) {
	iv, err := DecodeInvite(text)
	if err != nil {
		return Server{}, err
	}
	if alias == "" {
		alias = iv.Alias
	}
	if alias == "" {
		alias = "cc"
	}
	if !ValidAlias(alias) {
		return Server{}, fmt.Errorf("invalid alias %q", alias)
	}
	s := Server{Alias: alias, Host: iv.Host, Port: iv.Port, User: iv.User}
	pub, err := EnsureKey(alias)
	if err != nil {
		return s, err
	}
	sshDir, d, err := dirs()
	if err != nil {
		return s, err
	}
	kh := filepath.Join(sshDir, "known_hosts")
	for _, l := range []string{s.gwKnownName() + " " + iv.GwKey, s.containerKnownName() + " " + iv.UserKey} {
		if strings.Count(l, " ") < 2 {
			return s, errors.New("invite lacks host keys")
		}
		if err := appendLine(kh, l); err != nil {
			return s, err
		}
	}

	// Enroll through a private config that uses only the one-time key.
	tmpKey := filepath.Join(d, ".invite-"+iv.ID)
	if err := os.WriteFile(tmpKey, []byte(iv.Key), 0o600); err != nil {
		return s, err
	}
	defer os.Remove(tmpKey)
	tmpCfg := tmpKey + ".conf"
	cfg := fmt.Sprintf(`Host gw
    HostName %s
    Port %d
    User %s
    IdentityFile "%[4]s"
    IdentitiesOnly yes
    UserKnownHostsFile "%[5]s"
    StrictHostKeyChecking yes
    HostKeyAlias %[6]s
Host target
    HostName rcc-%[3]s
    Port 22
    User root
    ProxyJump gw
    IdentityFile "%[4]s"
    IdentitiesOnly yes
    UserKnownHostsFile "%[5]s"
    StrictHostKeyChecking yes
    HostKeyAlias %[7]s
`, iv.Host, iv.Port, iv.User, filepath.ToSlash(tmpKey), filepath.ToSlash(kh), s.gwKnownName(), s.containerKnownName())
	if err := os.WriteFile(tmpCfg, []byte(cfg), 0o600); err != nil {
		return s, err
	}
	defer os.Remove(tmpCfg)
	c := exec.Command("ssh", "-F", tmpCfg, "-o", "BatchMode=yes", "target")
	c.Stdin = strings.NewReader(pub + "\n")
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil || !strings.Contains(out.String(), "enrolled") {
		return s, fmt.Errorf("enrollment failed (the invite may be used, expired or revoked):\n%s", strings.TrimSpace(out.String()))
	}
	if err := Configure(s, iv.GwKey, iv.UserKey); err != nil {
		return s, err
	}
	if err := Verify(alias, iv.User); err != nil {
		return s, fmt.Errorf("enrolled, but %w", err)
	}
	return s, nil
}

// PrintInvite shows an invite (stdout) with instructions (stderr).
func PrintInvite(iv Invite, who, prog string) {
	fmt.Fprintf(os.Stderr, "\nOne-time invite for %s on %s (valid until %s server time). Send it privately:\n"+
		"whoever holds it can add one device for %s until it is used or expires.\n\n", who, iv.Host, iv.Expires, iv.User)
	fmt.Println(iv.Encode())
	fmt.Fprintf(os.Stderr, "\nOn the new machine (after installing remote-cc):\n  %s device join <invite>\n", prog)
}
