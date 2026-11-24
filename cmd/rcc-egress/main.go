//go:build linux

// Command rcc-egress is the egress host of a remote-cc server: it syncs proxy
// nodes from remote-claude-browser and gives each remote-cc session the
// network namespace of its node, whose only way out is that node.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
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
	every := flag.Duration("sync", 5*time.Minute, "node sync interval")
	flag.Parse()
	o.SyncEvery = *every
	log.SetPrefix("rcc-egress: ")
	if o.RCBURL != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			log.Fatalf("host token: %v", err)
		}
		o.Token = strings.TrimSpace(string(b))
	}
	if o.RCBURL == "" && o.NodesFile == "" {
		log.Print("no remote-claude-browser configured; serving cached nodes only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := egress.New(o).Run(ctx); err != nil {
		log.Fatal(err)
	}
}
