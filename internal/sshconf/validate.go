package sshconf

import (
	"bytes"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Everything written into ~/.ssh/remote-cc/config and ~/.ssh/known_hosts
// from outside (an invite, a server's answer) is checked here first: a
// newline or a quote there would add ssh options (ProxyCommand runs a
// command) or trusted keys (@cert-authority).

var (
	hostLabel = `[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?`
	hostRe    = regexp.MustCompile(`^` + hostLabel + `(\.` + hostLabel + `)*\.?$`)
	idRe      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	stampRe   = regexp.MustCompile(`^[0-9]{12}$`)
	// jumpRe is an ssh destination for ProxyJump: [user@]host[:port] or an
	// alias, comma-separated; nothing ssh_config would split or expand.
	jumpRe = regexp.MustCompile(`^[A-Za-z0-9_.@:\[\],+-]{1,255}$`)
)

// ValidHost checks a host name or IP address (IPv6 without brackets).
func ValidHost(h string) error {
	if ip := net.ParseIP(h); ip != nil && !strings.Contains(h, "%") {
		return nil
	}
	if len(h) > 253 || !hostRe.MatchString(h) {
		return fmt.Errorf("invalid host name %q", h)
	}
	return nil
}

// validPort checks a TCP port.
func validPort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("invalid port %d", p)
	}
	return nil
}

// validJump checks a ProxyJump destination.
func validJump(j string) error {
	if !jumpRe.MatchString(j) || strings.HasPrefix(j, "-") {
		return fmt.Errorf("cannot use %q as ProxyJump (write it as a Host entry in ~/.ssh/config and pass its name)", j)
	}
	return nil
}

// hostKeyTypes are the host key types a remote-cc server has.
var hostKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519: true, ssh.KeyAlgoECDSA256: true, ssh.KeyAlgoECDSA384: true, ssh.KeyAlgoECDSA521: true, ssh.KeyAlgoRSA: true,
}

// HostKey parses a host key "type base64" (a comment after it is dropped)
// and returns it in that form. Certificates and other key types fail.
func HostKey(s string) (string, error) {
	f := strings.Fields(s)
	if strings.ContainsAny(s, "\r\n\x00") || len(f) < 2 || len(f) > 3 || !hostKeyTypes[f[0]] {
		return "", errors.New("not a host key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", errors.New("not a host key")
	}
	pk, err := ssh.ParsePublicKey(blob)
	if err != nil || pk.Type() != f[0] {
		return "", errors.New("not a host key")
	}
	return pk.Type() + " " + base64.StdEncoding.EncodeToString(pk.Marshal()), nil
}

// validate checks what Configure writes into the ssh config.
func (s Server) validate() error {
	if !ValidAlias(s.Alias) {
		return fmt.Errorf("invalid alias %q", s.Alias)
	}
	if err := ValidUser(s.User); err != nil {
		return err
	}
	if err := ValidHost(s.Host); err != nil {
		return err
	}
	if err := validPort(s.Port); err != nil {
		return err
	}
	if s.Jump != "" {
		return validJump(s.Jump)
	}
	return nil
}

// Check validates every field of an invite and puts its host keys in
// canonical form. An invite is data from someone else: nothing in it may
// reach a config file or a file name unchecked.
func (iv *Invite) Check() error {
	if iv.V != 1 {
		return errors.New("unsupported invite version (update remote-cc)")
	}
	if err := ValidUser(iv.User); err != nil {
		return err
	}
	if err := ValidHost(iv.Host); err != nil {
		return err
	}
	if err := validPort(iv.Port); err != nil {
		return err
	}
	if iv.Alias != "" && !ValidAlias(iv.Alias) {
		return fmt.Errorf("invalid alias %q", iv.Alias)
	}
	if !idRe.MatchString(iv.ID) {
		return errors.New("invalid invite id")
	}
	if iv.Expires != "" && !stampRe.MatchString(iv.Expires) {
		return errors.New("invalid expiry time")
	}
	var err error
	if iv.GwKey, err = HostKey(iv.GwKey); err != nil {
		return fmt.Errorf("the gateway's host key: %w", err)
	}
	if iv.UserKey, err = HostKey(iv.UserKey); err != nil {
		return fmt.Errorf("the container's host key: %w", err)
	}
	return checkPrivateKey(iv.Key)
}

// checkPrivateKey accepts one unencrypted OpenSSH or PEM private key.
func checkPrivateKey(k string) error {
	block, rest := pem.Decode([]byte(k))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("invalid one-time key")
	}
	if _, err := ssh.ParsePrivateKey([]byte(k)); err != nil {
		return errors.New("invalid one-time key")
	}
	return nil
}
