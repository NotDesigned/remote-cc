package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"remote-cc/internal/remote"
)

// Known is a server this machine administers: the Docker host as reached
// for admin commands (setup-server, update-server, …). It is remembered so
// that `update-server --all` can visit every one.
type Known struct {
	Dest       string   `json:"dest"`
	Args       []string `json:"args,omitempty"` // ssh options given on the command line
	InstallDir string   `json:"install_dir"`
}

func (k Known) key() string { return k.Dest + "|" + k.InstallDir }

func knownPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".ssh", "remote-cc")
	return filepath.Join(d, "servers.json"), os.MkdirAll(d, 0o700)
}

// KnownServers lists the remembered servers.
func KnownServers() ([]Known, error) {
	p, err := knownPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ks []Known
	if err := json.Unmarshal(b, &ks); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return ks, nil
}

func saveKnown(ks []Known) error {
	p, err := knownPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ks, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// remember records c as a known server (best effort: a failure only warns).
func remember(c Config) {
	k := Known{Dest: c.Dest, Args: c.Args, InstallDir: c.InstallDir}
	ks, err := KnownServers()
	if err == nil {
		ks = slices.DeleteFunc(ks, func(x Known) bool { return x.key() == k.key() })
		err = saveKnown(append(ks, k))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcc: warning: could not remember this server for update-server --all: %v\n", err)
	}
}

// Forget drops remembered servers whose destination is dest (and install
// dir, if given). It reports how many were dropped.
func Forget(dest, installDir string) (int, error) {
	ks, err := KnownServers()
	if err != nil {
		return 0, err
	}
	n := len(ks)
	ks = slices.DeleteFunc(ks, func(x Known) bool {
		return x.Dest == dest && (installDir == "" || x.InstallDir == installDir)
	})
	if len(ks) == n {
		return 0, nil
	}
	return n - len(ks), saveKnown(ks)
}

// ListKnown prints the remembered servers.
func ListKnown() error {
	ks, err := KnownServers()
	if err != nil {
		return err
	}
	if len(ks) == 0 {
		fmt.Printf("no servers remembered yet; setup-server, update-server and server migrate add them\n")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tINSTALL DIR\tSSH OPTIONS")
	for _, k := range ks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.Dest, k.InstallDir, dash(strings.Join(k.Args, " ")))
	}
	return tw.Flush()
}

// UpdateAll runs Update on every remembered server, one after another, and
// reports each outcome. base supplies -v and similar settings.
func UpdateAll(base Config, image string, force bool) error {
	ks, err := KnownServers()
	if err != nil {
		return err
	}
	if len(ks) == 0 {
		return fmt.Errorf("no servers remembered yet: run `%s update-server <host>` once for each (setup-server and server migrate also remember them)", remote.Prog)
	}
	type outcome struct {
		k   Known
		err error
	}
	var res []outcome
	for i, k := range ks {
		fmt.Fprintf(os.Stderr, "\n=== [%d/%d] %s (%s)\n", i+1, len(ks), k.Dest, k.InstallDir)
		c := base
		c.Target = remote.Target{SSH: base.SSH, Args: k.Args, Dest: k.Dest, Verbose: base.Verbose}
		c.InstallDir = k.InstallDir
		err := Update(c, image, force)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rcc:", err)
		}
		res = append(res, outcome{k, err})
	}
	fmt.Println("\nsummary:")
	failed := 0
	for _, r := range res {
		st := "ok"
		if r.err != nil {
			st, failed = "FAILED: "+firstLine(r.err.Error()), failed+1
		}
		name := r.k.Dest
		if r.k.InstallDir != "/opt/remote-cc" {
			name += " " + r.k.InstallDir
		}
		fmt.Printf("  %-40s %s\n", name, st)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d servers failed (servers with active sessions are skipped unless --force)", failed, len(res))
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
