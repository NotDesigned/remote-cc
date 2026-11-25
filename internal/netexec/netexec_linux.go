//go:build linux

package netexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"remote-cc/internal/egress"
)

// Main runs a stage; args start with Stage1 or Stage2.
func Main(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		return fail("usage: %s -- PROGRAM [ARGS...]", args[0])
	}
	var err error
	switch args[0] {
	case Stage1:
		err = enter(args[2:])
	case Stage2:
		err = check(args[2:])
	default:
		err = errors.New("unknown stage")
	}
	return fail("%v", err)
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "rcc: "+format+"\n", a...)
	return 126
}

// enter gets the node's namespace, moves this thread into it, points the
// resolver at it and re-executes as stage 2: execve keeps the calling
// thread's namespace, so the whole new process is inside.
func enter(prog []string) error {
	node := os.Getenv(EnvNode)
	if node == "" {
		return fmt.Errorf("%s is not set", EnvNode)
	}
	sock := os.Getenv(EnvSock)
	if sock == "" {
		sock = DefaultSock
	}
	fd, reply, err := egress.Dial(sock, node, os.Getenv(EnvGrant), 40*time.Second)
	if err != nil {
		return fmt.Errorf("node %s: %w", node, err)
	}
	if resolv := os.Getenv(EnvResolv); resolv != "" {
		if err := os.WriteFile(resolv, []byte("nameserver "+reply.DNS+"\noptions timeout:3 attempts:2\n"), 0o644); err != nil {
			return err
		}
		if err := unix.Mount(resolv, "/etc/resolv.conf", "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("resolver for the node: %w (needs a private mount namespace)", err)
		}
	}
	runtime.LockOSThread() // never unlocked: this thread execs
	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("entering the node's network: %w", err)
	}
	unix.Close(fd)
	if os.Getenv(EnvLabel) == "" {
		os.Setenv(EnvLabel, reply.Name)
	}
	return syscall.Exec("/proc/self/exe", append([]string{"rcc-shim", Stage2, "--"}, prog...), os.Environ())
}

// Exit is what the exit lookup reports.
type Exit struct {
	IP       string
	City     string
	Country  string
	TimeZone string
}

func lookupExit(url string) (Exit, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Exit{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var v struct {
		IP       string `json:"ip"`
		City     string `json:"city"`
		Country  string `json:"country"`
		Timezone struct {
			ID string `json:"id"`
		} `json:"timezone"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.IP == "" {
		return Exit{}, fmt.Errorf("unexpected answer from %s (HTTP %d)", url, resp.StatusCode)
	}
	return Exit{v.IP, v.City, v.Country, v.Timezone.ID}, nil
}

// ExitAllowed reports whether ip is one of the comma-separated expected IPs.
func ExitAllowed(ip, expected string) bool {
	if strings.TrimSpace(expected) == "" {
		return true
	}
	for _, e := range strings.Split(expected, ",") {
		if strings.TrimSpace(e) == ip {
			return true
		}
	}
	return false
}

// AccountEmail reads `claude auth status --json`.
func AccountEmail(out []byte) (loggedIn bool, email string) {
	var st struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	json.Unmarshal(bytes.TrimSpace(out), &st)
	return st.LoggedIn, st.Email
}

// CheckAccount compares the account Claude Code is logged in as with the
// profile's. A stored setup-token logs in without an email, so that account
// cannot be compared: it passes with a warning.
func CheckAccount(loggedIn bool, got, want string) (warning string, err error) {
	switch {
	case !loggedIn:
		return "", fmt.Errorf("this Claude account is not logged in yet; log in as %s first (account login --rcb)", want)
	case got == "":
		return fmt.Sprintf("Claude Code reports no account email (a stored token?); not checked against the profile's %s", want), nil
	case !strings.EqualFold(got, want):
		return "", fmt.Errorf("Claude Code is logged in as %s, but the profile's browser is signed in as %s; refusing to start (log out and log in again with --rcb)", got, want)
	}
	return "", nil
}

// check runs inside the namespace: verify the exit (and with it that the
// node works at all), the Claude account, then exec the program.
func check(prog []string) error {
	label := os.Getenv(EnvLabel)
	url := os.Getenv(EnvCheckURL)
	if url == "" {
		url = "https://ipwho.is/"
	}
	exit, err := lookupExit(url)
	if err != nil {
		return fmt.Errorf("no network through node %s: %v", label, err)
	}
	if !ExitAllowed(exit.IP, os.Getenv(EnvExpectIP)) {
		// remote-cc re-checks the exit with remote-claude-browser and retries once.
		fmt.Fprintf(os.Stderr, "rcc: node %s exits at %s, but remote-claude-browser recorded %s for this profile; refusing to start\n",
			label, exit.IP, os.Getenv(EnvExpectIP))
		os.Exit(ExitMismatch)
	}
	tz := os.Getenv(EnvTZ)
	if tz == "" {
		tz = exit.TimeZone
	}
	if tz != "" {
		os.Setenv("TZ", tz)
	}
	if want := os.Getenv(EnvEmail); want != "" {
		out, _ := exec.Command(prog[0], "auth", "status", "--json").Output()
		in, got := AccountEmail(out)
		warn, err := CheckAccount(in, got, want)
		if err != nil {
			return err
		}
		if warn != "" {
			fmt.Fprintln(os.Stderr, "rcc: warning:", warn)
		}
	}
	fmt.Fprintf(os.Stderr, "rcc: network via node %s: exit %s (%s, %s), TZ=%s\n", label, exit.IP, exit.City, exit.Country, tz)
	for _, k := range []string{EnvSock, EnvNode, EnvGrant, EnvResolv, EnvExpectIP, EnvTZ, EnvEmail, EnvLabel, EnvCheckURL} {
		os.Unsetenv(k)
	}
	path, err := exec.LookPath(prog[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, prog, os.Environ())
}
