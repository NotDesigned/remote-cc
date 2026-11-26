// Package server implements the admin commands for a remote-cc server: a
// Docker host running one gateway container (the only public port, stock
// OpenSSH) and one container per user on a private network, reached with the
// gateway as ProxyJump. Every command runs a script from internal/scripts on
// the host as root (directly, or through sudo).
package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"remote-cc/deploy"
	"remote-cc/internal/rcb"
	"remote-cc/internal/remote"
	"remote-cc/internal/scripts"
	"remote-cc/internal/sshconf"
)

// Config selects a server. Zero values mean the defaults below.
type Config struct {
	remote.Target        // the Docker host: root or an account with sudo
	InstallDir    string // /opt/remote-cc
	DataDir       string // /var/lib/remote-cc (init only; later read from .env)
	Port          int    // 2222
	Bind          string // "" = all interfaces, IPv4 and IPv6
	Image         string // ghcr.io/notdesigned/remote-cc-server:latest
	PublicHost    string // address users connect to; default: what ssh resolves Dest to
	Name          string // instance: gateway "<name>-gateway", network "<name>"; default remote-cc
	InstallDocker bool
}

func (c *Config) defaults() {
	if c.InstallDir == "" {
		c.InstallDir = "/opt/remote-cc"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/remote-cc"
	}
	if c.Port == 0 {
		c.Port = 2222
	}
	if c.Image == "" {
		c.Image = "ghcr.io/notdesigned/remote-cc-server:latest"
	}
	if c.Name == "" {
		c.Name = "remote-cc"
	}
}

func (c *Config) publicHost() string {
	if c.PublicHost != "" {
		return c.PublicHost
	}
	return remote.Hostname(c.Target)
}

func (c *Config) open() (*remote.Conn, error) {
	c.defaults()
	return remote.Open(c.Target)
}

func (c *Config) initParams() string {
	return remote.Vars("DIR", c.InstallDir, "DATA", c.DataDir, "PORT", strconv.Itoa(c.Port), "BIND", c.Bind,
		"IMAGE", c.Image, "GW", c.Name+"-gateway", "NET", c.Name, "INSTALL_DOCKER", remote.Bool(c.InstallDocker),
		"COMPOSE_B64", remote.Base64(deploy.ComposeGateway))
}

func (c *Config) addParams(user string, keys []string) string {
	return remote.Vars("DIR", c.InstallDir, "NAME", user, "KEYS_B64", remote.Base64(strings.Join(keys, "\n")),
		"COMPOSE_B64", remote.Base64(deploy.ComposeUser))
}

// Setup installs (or refreshes) a server with the caller as its first user:
// gateway, user, a key for that server on this machine, the ssh config entry
// and pinned host keys — init + server user add + device join in one step.
func Setup(c Config, user, alias string, extraKeys []string) error {
	if user == "" {
		user = "owner"
	}
	if err := sshconf.ValidUser(user); err != nil {
		return err
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	host := c.publicHost()
	if alias == "" {
		alias = sshconf.DefaultAlias(host)
	}
	if !sshconf.ValidAlias(alias) {
		return fmt.Errorf("invalid alias %q", alias)
	}
	pub, err := sshconf.EnsureKey(alias)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rcc: setting up remote-cc on %s (port %d, user %s) …\n", c.Dest, c.Port, user)
	f, err := conn.Elevated(c.initParams(), scripts.Host("server/init.sh"))
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	remember(c)
	f2, err := conn.Elevated(c.addParams(user, append([]string{pub}, extraKeys...)), scripts.Host("server/user-add.sh"))
	if err != nil {
		return fmt.Errorf("setup failed while creating user %s: %w", user, err)
	}
	port, _ := strconv.Atoi(f["port"])
	s := sshconf.Server{Alias: alias, Host: host, Port: port, User: user}
	if loopback(c.Bind) || !reachable(host, port) {
		s.Jump = c.Dest
		if !loopback(c.Bind) {
			fmt.Fprintf(os.Stderr, "rcc: port %d on %s is not reachable from here (cloud firewall?); going through %s.\n"+
				"     Open TCP %d to connect directly, then run server setup again.\n", port, host, c.Dest, port)
		}
		if len(c.Args) > 0 {
			fmt.Fprintf(os.Stderr, "rcc: note: ProxyJump uses %q as written; put the ssh options you passed into a Host entry for it.\n", c.Dest)
		}
	}
	if err := sshconf.Configure(s, f["gwkey"], f2["ukey"]); err != nil {
		return err
	}
	if err := sshconf.Verify(alias, user); err != nil {
		return fmt.Errorf("server is up, but %w", err)
	}
	fmt.Printf(`
remote-cc is ready on %[1]s: you are user %[2]s, reachable as "%[3]s" (ssh config in ~/.ssh/remote-cc/config).
  %[4]s account login %[3]s  # sign in to Claude (once)
  %[4]s %[3]s                # in a project directory
  %[4]s server user add %[5]s <name>   # add people (prints a one-time invite)
`, host, user, alias, remote.Prog, c.Dest)
	return nil
}

// Init installs only the gateway.
func Init(c Config) error {
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	f, err := conn.Elevated(c.initParams(), scripts.Host("server/init.sh"))
	if err != nil {
		return fmt.Errorf("server init failed: %w", err)
	}
	remember(c)
	fmt.Printf("gateway is running on port %s, host key %s\nadd users with:  %s server user add %s <name>\n",
		f["port"], sshconf.Fingerprint(f["gwkey"]), remote.Prog, c.Dest)
	return nil
}

// UserAdd creates a user (or adds keys to one). Without keys it prints an
// invite for them instead.
func UserAdd(c Config, name string, keyFiles []string, alias string) error {
	if err := sshconf.ValidUser(name); err != nil {
		return err
	}
	var keys []string
	for _, kf := range keyFiles {
		b, err := os.ReadFile(kf)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); sshconf.Fingerprint(l) != "" && !strings.HasPrefix(l, "#") {
				keys = append(keys, l)
			}
		}
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	f, err := conn.Elevated(c.addParams(name, keys), scripts.Host("server/user-add.sh"))
	if err != nil {
		return fmt.Errorf("server user add failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "rcc: user %s is ready (container rcc-%s, %s key(s))\n", name, name, f["keys"])
	if len(keys) == 0 {
		return invite(conn, c, name, alias, 7*24*time.Hour)
	}
	printConfig(c, name, alias, f)
	return nil
}

// UserInvite prints a one-time invite for an existing user.
func UserInvite(c Config, name, alias string, ttl time.Duration) error {
	if err := sshconf.ValidUser(name); err != nil {
		return err
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	return invite(conn, c, name, alias, ttl)
}

func invite(conn *remote.Conn, c Config, name, alias string, ttl time.Duration) error {
	id := remote.RandHex(8)
	priv, pub, err := sshconf.NewKey("")
	if err != nil {
		return err
	}
	hours := max(1, int(ttl.Hours()))
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "NAME", name, "ID", id, "PUB", pub, "TTL", strconv.Itoa(hours)),
		scripts.Host("server/user-invite.sh"))
	if err != nil {
		return fmt.Errorf("invite failed: %w", err)
	}
	host := c.publicHost()
	if alias == "" {
		alias = sshconf.DefaultAlias(host)
	}
	port, _ := strconv.Atoi(f["port"])
	iv := sshconf.Invite{V: 1, Host: host, Port: port, User: name, Alias: alias, GwKey: f["gwkey"], UserKey: f["ukey"],
		ID: id, Key: string(priv), Expires: f["exp"]}
	return sshconf.PrintInvite(iv, name, remote.Prog)
}

// UserConfig prints the ssh config a user with their own key would use.
func UserConfig(c Config, name, alias string) error {
	if err := sshconf.ValidUser(name); err != nil {
		return err
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "NAME", name), scripts.Host("server/user-info.sh"))
	if err != nil {
		return err
	}
	printConfig(c, name, alias, f)
	return nil
}

func printConfig(c Config, name, alias string, f map[string]string) {
	host := c.publicHost()
	if alias == "" {
		alias = sshconf.DefaultAlias(host)
	}
	port, _ := strconv.Atoi(f["port"])
	gwName := host
	if port != 22 {
		gwName = fmt.Sprintf("[%s]:%d", host, port)
	}
	uk := fmt.Sprintf("rcc-%s@%s", name, host)
	fmt.Printf(`
Simplest for %[1]s: an invite (%[9]s server user invite %[10]s %[1]s), then "%[9]s device join <invite>".
Or by hand, with the private key whose public key was authorized — ~/.ssh/config:

  Host %[2]s-gw
      HostName %[3]s
      Port %[4]d
      User %[1]s
      IdentityFile ~/.ssh/id_ed25519
  Host %[2]s
      HostName rcc-%[1]s
      User root
      ProxyJump %[2]s-gw
      HostKeyAlias %[5]s
      IdentityFile ~/.ssh/id_ed25519
      ServerAliveInterval 15

and ~/.ssh/known_hosts:

  %[6]s %[7]s
  %[5]s %[8]s
`, name, alias, host, port, uk, gwName, f["gwkey"], f["ukey"], remote.Prog, c.Dest)
}

// UserList prints the users of a server.
func UserList(c Config) error {
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	out, err := conn.ElevatedOutput(remote.Vars("DIR", c.InstallDir), scripts.Host("server/user-list.sh"))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tCONTAINER\tKEYS\tACTIVE SESSIONS\tDATA")
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "user="); ok {
			fmt.Fprintln(tw, strings.ReplaceAll(v, "|", "\t"))
			n++
		}
	}
	tw.Flush()
	if n == 0 {
		fmt.Printf("no users yet: %s server user add %s <name>\n", remote.Prog, c.Dest)
	}
	return nil
}

// UserRebuild recreates a user's container (data, keys and settings kept).
func UserRebuild(c Config, name string, force bool) error {
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "rcc: rebuilding rcc-%s on %s …\n", name, c.Dest)
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "NAME", name, "FORCE", remote.Bool(force)),
		scripts.Host("server/user-rebuild.sh"))
	if err != nil {
		return fmt.Errorf("server user rebuild failed: %w", err)
	}
	fmt.Printf("rebuilt rcc-%s (%s); its data, keys and settings are kept\n", name, f["image"])
	return nil
}

// UserRemove revokes a user's access and removes their container.
func UserRemove(c Config, name string, purge, force bool) error {
	if err := sshconf.ValidUser(name); err != nil {
		return err
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "NAME", name, "PURGE", remote.Bool(purge), "FORCE", remote.Bool(force)),
		scripts.Host("server/user-remove.sh")); err != nil {
		return fmt.Errorf("server user remove failed: %w", err)
	}
	fmt.Printf("user %s removed (access revoked)\n", name)
	return nil
}

// Migrate converts a single-container deployment of rcc <= 0.4.0 into a
// server whose first user is its previous owner.
func Migrate(c Config, name, alias string, force bool) error {
	if err := sshconf.ValidUser(name); err != nil {
		return err
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "rcc: converting %s (first user: %s) …\n", c.Dest, name)
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "NAME", name, "IMAGE", c.Image, "FORCE", remote.Bool(force),
		"GW", c.Name+"-gateway", "NET", c.Name,
		"COMPOSE_GW_B64", remote.Base64(deploy.ComposeGateway), "COMPOSE_USER_B64", remote.Base64(deploy.ComposeUser)),
		scripts.Host("server/migrate.sh"))
	if f["backup"] != "" {
		fmt.Fprintf(os.Stderr, "rcc: backup of the previous data: %s (on the server)\n", f["backup"])
	}
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "rcc: done. %s is now user %s (%s key(s) carried over); the server keeps its host key.\n", c.Dest, name, f["keys"])
	remember(c)
	printConfig(c, name, alias, f)
	return nil
}

// Update pulls the latest images and recreates containers that changed,
// the egress host's too when one is installed (same token and settings).
func Update(c Config, image string, force bool) error {
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "rcc: updating remote-cc server on %s …\n", c.Dest)
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "IMAGE", image, "FORCE", remote.Bool(force),
		"COMPOSE_GW_B64", remote.Base64(deploy.ComposeGateway), "COMPOSE_USER_B64", remote.Base64(deploy.ComposeUser),
		"COMPOSE_EGRESS_B64", remote.Base64(deploy.ComposeEgress)),
		scripts.Host("server/update.sh"))
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	remember(c)
	if f["started"] != "" {
		fmt.Printf("started stopped container(s): %s\n", f["started"])
	}
	switch {
	case f["uptodate"] == "1":
		fmt.Printf("remote-cc server is up to date (%s)\n", f["before"])
	case f["recreated"] != "1":
		fmt.Printf("remote-cc server is up to date (%s); its compose files were refreshed\n", f["after"])
	default:
		fmt.Printf("remote-cc server updated: %s -> %s\n", dash(f["before"]), dash(f["after"]))
	}
	if f["egress"] == "updated" {
		fmt.Println("egress host updated (new image or compose file)")
	}
	return nil
}

// Remove deletes the gateway and all user containers (data kept unless purge).
func Remove(c Config, purge bool) error {
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "PURGE", remote.Bool(purge)), scripts.Host("server/remove.sh"))
	if err != nil {
		return fmt.Errorf("remove failed: %w", err)
	}
	Forget(c.Dest, c.InstallDir)
	host := c.publicHost()
	name := host
	if f["port"] != "" && f["port"] != "22" {
		name = "[" + host + "]:" + f["port"]
	}
	// The HOSTs on this machine for this server: their config entries and
	// pinned host keys (gateway and containers) would only mislead now.
	port, _ := strconv.Atoi(f["port"])
	if port == 0 {
		port = 22
	}
	gone, err := sshconf.Forget(host, port)
	if len(gone) > 0 {
		fmt.Fprintf(os.Stderr, "rcc: removed %s from ~/.ssh/remote-cc/config and their host keys from your known_hosts\n", strings.Join(gone, ", "))
	}
	if err != nil {
		return fmt.Errorf("server removed, but cleaning up this machine's ssh files failed: %w", err)
	}
	// Also a hashed entry ssh-keygen -H made of the gateway's name.
	if exec.Command("ssh-keygen", "-R", name).Run() == nil {
		fmt.Fprintf(os.Stderr, "rcc: removed %s from your known_hosts\n", name)
	}
	return nil
}

func loopback(bind string) bool { return bind == "127.0.0.1" || bind == "localhost" || bind == "::1" }

func reachable(host string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 6*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Egress installs or updates the egress host: Claude Code sessions started
// with --rcb then run in the network namespace of their browser profile's
// proxy node. The host token comes from remote-claude-browser at rcbURL,
// approved there by an administrator (or RCC_EGRESS_TOKEN).
// image "" = the published one.
func Egress(c Config, rcbURL, image string) error {
	base, err := rcb.NormalizeURL(rcbURL)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(os.Getenv("RCC_EGRESS_TOKEN"))
	if token == "" {
		c.defaults()
		name := c.publicHost()
		if c.Name != "remote-cc" {
			name += " (" + c.Name + ")"
		}
		token, err = rcb.Connect(base, "host", name, func(code, link string) {
			fmt.Fprintf(os.Stderr, "rcc: an administrator of remote-claude-browser approves this egress host:\n\n  %s\n\n  connection code: %s\n\nrcc: waiting for approval …\n", link, code)
		})
		if err != nil {
			return err
		}
	}
	if !strings.HasPrefix(token, "rcch_") {
		return errors.New("that is not a host token (they start with rcch_)")
	}
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "rcc: installing the egress host on %s …\n", c.Dest)
	out, err := conn.ElevatedInput(remote.Vars("DIR", c.InstallDir, "URL", base, "EIMAGE", image,
		"COMPOSE_B64", remote.Base64(deploy.ComposeEgress), "COMPOSE_USER_B64", remote.Base64(deploy.ComposeUser)),
		scripts.Host("server/egress.sh"), token+"\n")
	if err != nil {
		return fmt.Errorf("egress setup failed: %w", err)
	}
	f := remote.ParseFacts(out)
	state := "already up to date"
	if f["recreated"] == "1" {
		state = "started with the current image and settings"
	}
	fmt.Printf("egress host is running on %s, linked to %s (%s)\n", c.Dest, base, state)
	if a := strings.TrimSpace(f["attached"]); a != "" {
		fmt.Printf("attached to it: %s\n", a)
	}
	if b := strings.TrimSpace(f["busy"]); b != "" {
		fmt.Printf("still to attach (sessions running): %s\n  when they end, run: %s server update %s\n", b, remote.Prog, c.Dest)
	}
	return nil
}
