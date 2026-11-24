package client

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"remote-cc/internal/remote"
	"remote-cc/internal/scripts"
	"remote-cc/internal/sshconf"
)

// Self-service devices: a machine already enrolled as a user can invite more
// machines of the same user, and list or remove that user's keys, without the
// admin. The invite's gateway half is registered by the gateway's
// rcc-self-invite (only ever for the calling account); its container half is
// written here, inside the user's own container.

// DeviceInvite prints a one-time invite for another machine of this user.
func DeviceInvite(alias string, ttl time.Duration) error {
	srv, err := sshconf.Lookup(alias)
	if err != nil {
		return err
	}
	priv, pub, err := sshconf.NewKey("")
	if err != nil {
		return err
	}
	id := remote.RandHex(8)
	hours := min(168, max(1, int(ttl.Hours())))
	f := strings.Fields(pub) // "ssh-ed25519 <base64>"

	gw, err := remote.Open(remote.Target{Dest: alias + "-gw"})
	if err != nil {
		return err
	}
	defer gw.Close()
	run := func(cmd string) (map[string]string, string, error) {
		c := gw.Raw(cmd)
		c.Stderr = os.Stderr
		out, err := c.Output()
		return remote.ParseFacts(out), strings.TrimSpace(string(out)), err
	}
	gf, _, err := run(fmt.Sprintf("invite %s %s %s %d", f[0], f[1], id, hours))
	if err != nil || gf["exp"] == "" {
		return fmt.Errorf("the gateway refused the invite (servers before rcc 0.5 need the admin: user invite): %v", err)
	}
	_, gwKey, err := run("hostkey")
	if err != nil {
		return err
	}

	ctr, err := remote.Open(remote.Target{Dest: alias})
	if err != nil {
		return err
	}
	defer ctr.Close()
	cf, err := ctr.Facts(remote.Vars("PUB", pub, "ID", id, "EXP", gf["exp"]) + scripts.Get("device/invite.sh"))
	if err != nil {
		return err
	}
	iv := sshconf.Invite{V: 1, Host: srv.Host, Port: srv.Port, User: srv.User, Alias: alias, GwKey: gwKey,
		UserKey: cf["ukey"], ID: id, Key: string(priv), Expires: gf["exp"]}
	sshconf.PrintInvite(iv, "another machine of "+srv.User, remote.Prog)
	return nil
}

type keyLine struct {
	line, fp, comment string
	invite            bool
	expires           string
	mine              bool
}

var expiryRe = regexp.MustCompile(`expiry-time="([0-9]{12})"`)

func readKeys(alias string) (*remote.Conn, string, []keyLine, error) {
	c, err := remote.Open(remote.Target{Dest: alias})
	if err != nil {
		return nil, "", nil, err
	}
	cmd := c.Script(false, nil, scripts.Get("device/keys.sh"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		c.Close()
		return nil, "", nil, err
	}
	var mineFP string
	if p, err := sshconf.KeyPath(alias); err == nil {
		if b, err := os.ReadFile(p + ".pub"); err == nil {
			mineFP = sshconf.Fingerprint(strings.TrimSpace(string(b)))
		}
	}
	var sha string
	var keys []keyLine
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if v, ok := strings.CutPrefix(l, "@@sha="); ok {
			sha = v
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		k := keyLine{line: l, fp: sshconf.Fingerprint(l)}
		f := strings.Fields(l)
		if len(f) > 0 {
			k.comment = f[len(f)-1]
		}
		if m := expiryRe.FindStringSubmatch(l); m != nil && strings.Contains(l, "rcc-invite-") {
			k.invite, k.expires = true, m[1]
		}
		k.mine = k.fp != "" && k.fp == mineFP
		keys = append(keys, k)
	}
	if sha == "" {
		c.Close()
		return nil, "", nil, errors.New("unexpected answer from the container")
	}
	return c, sha, keys, nil
}

// DeviceList prints this user's devices (keys) and pending invites.
func DeviceList(alias string) error {
	c, _, keys, err := readKeys(alias)
	if err != nil {
		return err
	}
	defer c.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE (KEY COMMENT)\tFINGERPRINT\tNOTE")
	for _, k := range keys {
		note := ""
		switch {
		case k.invite:
			note = "pending invite, expires " + fmtStamp(k.expires)
		case k.mine:
			note = "this machine"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", dash(k.comment), dash(k.fp), note)
	}
	return tw.Flush()
}

// DeviceRemove removes this user's keys (and invites) whose comment or
// fingerprint matches which. Removing this machine needs force; removing the
// last device is refused.
func DeviceRemove(alias, which string, force bool) error {
	c, sha, keys, err := readKeys(alias)
	if err != nil {
		return err
	}
	defer c.Close()
	var keep, drop []keyLine
	for _, k := range keys {
		if which != "" && (k.comment == which || k.fp == which || (strings.HasPrefix(which, "SHA256:") && strings.HasPrefix(k.fp, which))) {
			drop = append(drop, k)
		} else {
			keep = append(keep, k)
		}
	}
	if len(drop) == 0 {
		return fmt.Errorf("no device matches %q (see: %s device list %s)", which, remote.Prog, alias)
	}
	left := 0
	for _, k := range keep {
		if !k.invite {
			left++
		}
	}
	for _, k := range drop {
		if k.mine && !force {
			return errors.New("that is this machine's key; pass --force to remove it anyway (this machine then loses access)")
		}
	}
	if left == 0 {
		return errors.New("that would remove every device of this user; the admin can do that (user remove)")
	}
	var b strings.Builder
	for _, k := range keep {
		b.WriteString(k.line + "\n")
	}
	cmd := c.Script(false, nil, remote.Vars("OLD", sha)+scripts.Get("device/keys-put.sh"))
	cmd.Stdin = strings.NewReader(b.String())
	cmd.Stderr = os.Stderr
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), "saved") {
		return fmt.Errorf("could not save the key list: %v", err)
	}
	for _, k := range drop {
		fmt.Printf("removed %s %s\n", dash(k.comment), k.fp)
	}
	return nil
}

func fmtStamp(s string) string {
	if len(s) != 12 {
		return s
	}
	if _, err := strconv.Atoi(s); err != nil {
		return s
	}
	return s[0:4] + "-" + s[4:6] + "-" + s[6:8] + " " + s[8:10] + ":" + s[10:12]
}
