//go:build linux

// Command rcc-egress is the egress host of a remote-cc server: each remote-cc
// session presents a grant from remote-claude-browser, rcc-egress redeems it
// for the session's proxy node and hands over that node's network namespace,
// whose only way out is the node.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"remote-cc/internal/egress"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

func main() {
	o := egress.Options{}
	tokenFile := ""
	flag.StringVar(&o.RCBURL, "rcb", env("RCC_EGRESS_RCB_URL", ""), "remote-claude-browser origin")
	flag.StringVar(&tokenFile, "token-file", env("RCC_EGRESS_TOKEN_FILE", "/etc/rcc-egress/token"), "host token file")
	flag.StringVar(&o.NodesFile, "nodes-file", env("RCC_EGRESS_NODES_FILE", ""), "static node list (testing)")
	flag.StringVar(&o.StateDir, "state", env("RCC_EGRESS_STATE", "/var/lib/rcc-egress"), "state directory")
	flag.StringVar(&o.UsersDir, "users", env("RCC_EGRESS_USERS", "/run/rcc-egress/users"), "per-user socket directories")
	flag.StringVar(&o.Mihomo, "mihomo", env("RCC_EGRESS_MIHOMO", "/usr/local/bin/mihomo"), "Mihomo binary")
	flag.StringVar(&o.BootstrapDNS, "bootstrap-dns", env("RCC_EGRESS_BOOTSTRAP_DNS", ""), "resolver for node hostnames")
	every := flag.Duration("refresh", 5*time.Minute, "how often running nodes are re-checked")
	flag.IntVar(&o.MaxRuntimes, "max-nodes", envInt("RCC_EGRESS_MAX_NODES", 64), "node namespaces (each with a Mihomo) run at once")
	flag.IntVar(&o.MaxPerUser, "max-nodes-per-user", envInt("RCC_EGRESS_MAX_NODES_PER_USER", 8), "of them per remote-cc user; the least recently used makes room")
	flag.Parse()
	o.RefreshEvery = *every
	log.SetPrefix("rcc-egress: ")
	if o.RCBURL != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			log.Fatalf("host token: %v", err)
		}
		o.Token = strings.TrimSpace(string(b))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := egress.New(o).Run(ctx); err != nil {
		log.Fatal(err)
	}
}
