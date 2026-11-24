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
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"remote-cc/deploy"
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
// and pinned host keys — init + user add + join in one step.
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
				"     Open TCP %d to connect directly, then run setup-server again.\n", port, host, c.Dest, port)
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
  %[4]s login %[3]s          # sign in to Claude (once)
  %[4]s %[3]s                # in a project directory
  %[4]s user add %[5]s <name>   # add people (prints a one-time invite)
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
	fmt.Printf("gateway is running on port %s, host key %s\nadd users with:  %s user add %s <name>\n",
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
		return fmt.Errorf("user add failed: %w", err)
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
	sshconf.PrintInvite(iv, name, remote.Prog)
	return nil
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
Simplest for %[1]s: an invite (%[9]s user invite %[10]s %[1]s), then "%[9]s join <invite>".
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
		fmt.Printf("no users yet: %s user add %s <name>\n", remote.Prog, c.Dest)
	}
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
		return fmt.Errorf("user remove failed: %w", err)
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

// Update pulls the latest image and recreates containers that changed.
func Update(c Config, image string, force bool) error {
	conn, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "rcc: updating remote-cc server on %s …\n", c.Dest)
	f, err := conn.Elevated(remote.Vars("DIR", c.InstallDir, "IMAGE", image, "FORCE", remote.Bool(force),
		"COMPOSE_GW_B64", remote.Base64(deploy.ComposeGateway), "COMPOSE_USER_B64", remote.Base64(deploy.ComposeUser)),
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
// proxy node, synced from remote-claude-browser at rcbURL with a host token
// (asked for here, or RCC_EGRESS_TOKEN). image "" = the published one.
func Egress(c Config, rcbURL, image string) error {
	u, err := url.Parse(rcbURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("--rcb needs the remote-claude-browser address, e.g. https://browser.example.com")
	}
	token := strings.TrimSpace(os.Getenv("RCC_EGRESS_TOKEN"))
	if token == "" {
		token, err = remote.ReadPassword("host token (remote-claude-browser: 代理与 API → remote-cc → 出口主机): ")
		if err != nil {
			return err
		}
		token = strings.TrimSpace(token)
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
	out, err := conn.ElevatedInput(remote.Vars("DIR", c.InstallDir, "URL", strings.TrimRight(u.String(), "/"),
		"EIMAGE", image, "COMPOSE_B64", remote.Base64(deploy.ComposeEgress)), scripts.Host("server/egress.sh"), token+"\n")
	if err != nil {
		return fmt.Errorf("egress setup failed: %w", err)
	}
	f := remote.ParseFacts(out)
	switch sync := f["sync"]; {
	case strings.Contains(sync, "synced"):
		fmt.Printf("egress host is running: %s\n", sync[strings.Index(sync, "synced"):])
	case sync != "":
		fmt.Printf("egress host is running, but the first sync failed:\n  %s\n", sync)
	default:
		fmt.Println("egress host is running (no sync result yet; see: docker logs " + c.Name + "-egress)")
	}
	if m := strings.TrimSpace(f["missing"]); m != "" {
		fmt.Printf("these users' containers need the new mount: %s\n  run: %s update-server %s\n", m, remote.Prog, c.Dest)
	}
	return nil
}
