package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"remote-cc/internal/remote"
	"remote-cc/internal/server"
	"remote-cc/internal/sshconf"
)

// serverConfig is the server a command names: the ssh target (its first
// positional argument, if any) and whichever server options it takes.
func serverConfig(in *invocation) (server.Config, error) {
	c := server.Config{Target: in.target, InstallDir: in.s("--install-dir"), DataDir: in.s("--data"), Image: in.s("--image"),
		PublicHost: in.s("--public-host"), Name: in.s("--name"), Bind: in.s("--bind"), InstallDocker: in.b("--install-docker")}
	if len(in.pos) > 0 {
		c.Dest = in.pos[0]
	}
	if port := in.s("--port"); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return c, errors.New("bad --port")
		}
		c.Port = p
	}
	return c, nil
}

// admin wraps a server command: a bad option is a usage error.
func admin(run func(in *invocation, c server.Config) error) func(*invocation) int {
	return func(in *invocation) int {
		c, err := serverConfig(in)
		if err != nil {
			return usageError(in.cmd, err)
		}
		err = run(in, c)
		var bad badUsage
		if errors.As(err, &bad) {
			return usageError(in.cmd, bad.error)
		}
		return fail(err)
	}
}

var (
	runSetupServer = admin(func(in *invocation, c server.Config) error {
		var extra []string
		for _, kf := range in.l("--key") {
			b, err := os.ReadFile(kf)
			if err != nil {
				return err
			}
			for _, l := range strings.Split(string(b), "\n") {
				if l = strings.TrimSpace(l); sshconf.Fingerprint(l) != "" {
					extra = append(extra, l)
				}
			}
		}
		return server.Setup(c, in.s("--user"), in.s("--alias"), extra)
	})
	runServerInit   = admin(func(_ *invocation, c server.Config) error { return server.Init(c) })
	runServerEgress = admin(func(in *invocation, c server.Config) error {
		if in.s("--rcb") == "" {
			return badUsage{errors.New("server egress needs --rcb URL (your remote-claude-browser)")}
		}
		return server.Egress(c, in.s("--rcb"), c.Image)
	})
	runServerMigrate = admin(func(in *invocation, c server.Config) error {
		if in.s("--user") == "" {
			return badUsage{errors.New("server migrate needs --user NAME (the name the current owner gets)")}
		}
		return server.Migrate(c, in.s("--user"), in.s("--alias"), in.b("--force"))
	})
	runUpdateServer = admin(func(in *invocation, c server.Config) error {
		switch {
		case in.b("--all") && len(in.pos) != 0:
			return badUsage{errors.New("server update --all takes no SERVER")}
		case in.b("--all"):
			return server.UpdateAll(c, c.Image, in.b("--force"))
		case len(in.pos) == 0:
			return badUsage{errors.New("server update needs SERVER, or --all")}
		}
		return server.Update(c, c.Image, in.b("--force"))
	})
	runRemoveServer = admin(func(in *invocation, c server.Config) error { return server.Remove(c, in.b("--purge")) })
	runServers      = admin(func(in *invocation, c server.Config) error {
		forget := in.s("--forget")
		if forget == "" {
			return server.ListKnown()
		}
		n, err := server.Forget(forget, c.InstallDir)
		if err == nil && n == 0 {
			err = fmt.Errorf("no remembered server %q (see: %s server list)", forget, remote.Prog)
		}
		return err
	})

	runUserList = admin(func(_ *invocation, c server.Config) error { return server.UserList(c) })
	runUserAdd  = admin(func(in *invocation, c server.Config) error {
		return server.UserAdd(c, in.pos[1], in.l("--key"), in.s("--alias"))
	})
	runUserInvite = admin(func(in *invocation, c server.Config) error {
		d := 7 * 24 * time.Hour
		if ttl := in.s("--ttl"); ttl != "" {
			var err error
			if d, err = time.ParseDuration(ttl); err != nil {
				return badUsage{errors.New("bad --ttl (e.g. 24h, 168h)")}
			}
		}
		return server.UserInvite(c, in.pos[1], in.s("--alias"), d)
	})
	runUserConfig = admin(func(in *invocation, c server.Config) error {
		return server.UserConfig(c, in.pos[1], in.s("--alias"))
	})
	runUserRebuild = admin(func(in *invocation, c server.Config) error {
		return server.UserRebuild(c, in.pos[1], in.b("--force"))
	})
	runUserRemove = admin(func(in *invocation, c server.Config) error {
		return server.UserRemove(c, in.pos[1], in.b("--purge"), in.b("--force"))
	})
)
