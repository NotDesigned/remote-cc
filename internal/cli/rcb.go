package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"remote-cc/internal/rcb"
	"remote-cc/internal/remote"
)

func runRCBLogin(in *invocation) int {
	u, err := rcb.NormalizeURL(in.pos[0])
	if err != nil {
		return fail(err)
	}
	tok := strings.TrimSpace(os.Getenv("RCC_RCB_TOKEN"))
	if tok == "" {
		name, _ := os.Hostname()
		if tok, err = rcb.Connect(u, "client", name, func(code, link string) {
			fmt.Fprintf(os.Stderr, "Approve this computer in remote-claude-browser (you need to be logged in there):\n\n  %s\n\n  connection code: %s\n\nwaiting for approval …\n", link, code)
			openBrowser(link)
		}); err != nil {
			return fail(err)
		}
	}
	if !strings.HasPrefix(tok, "rcck_") {
		return fail(errors.New("that is not a client token (they start with rcck_)"))
	}
	l := rcb.Link{URL: u, Token: tok}
	ps, err := rcb.Profiles(l)
	if err != nil {
		return fail(err)
	}
	if err := rcb.Save(l); err != nil {
		return fail(err)
	}
	fmt.Printf("linked to %s: %d profile(s)\n  %s rcb profiles            # list them\n  %s rcb use PROFILE         # in a project directory; then just: %s\n", u, len(ps), remote.Prog, remote.Prog, remote.Prog)
	return 0
}

// linkedProfiles is the profiles of the linked remote-claude-browser.
func linkedProfiles() ([]rcb.Profile, error) {
	l, err := rcb.Load()
	if err != nil {
		return nil, err
	}
	return rcb.Profiles(l)
}

func runRCBUse(in *invocation) int {
	ps, err := linkedProfiles()
	if err != nil {
		return fail(err)
	}
	var p rcb.Profile
	if len(in.pos) == 0 {
		fmt.Fprintln(os.Stderr, "your profiles:")
		p, err = pickProfile(ps)
	} else {
		p, err = chooseProfile(ps, in.pos[0])
	}
	if err != nil {
		return fail(err)
	}
	if err := p.Usable(); err != nil {
		return fail(err)
	}
	host := ""
	if len(in.pos) == 2 {
		host = in.pos[1]
	} else if _, host, err = rcbDefaults(p.ID); err != nil {
		return fail(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	if err := rcb.Remember(cwd, rcb.Use{Profile: p.ID, Server: host}); err != nil {
		return fail(err)
	}
	fmt.Printf("%s now runs as profile %s on %s.\n", cwd, p.Label(), host)
	fmt.Printf("  first time: %s account login --rcb %s     # sign in once, in the profile's browser\n", remote.Prog, p.ID)
	fmt.Printf("  then:       %s                     # here and in subdirectories\n", remote.Prog)
	return 0
}

func runRCBProfiles(*invocation) int {
	ps, err := linkedProfiles()
	if err != nil {
		return fail(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tID\tNAME\tCLAUDE ACCOUNT\tNODE\tEXIT\tTIME ZONE")
	for i, p := range ps {
		node, ip, tz := "-", "-", "-"
		if p.Node != nil {
			node = p.Node.Name
		}
		if p.Exit != nil {
			ip, tz = p.Exit.IP, p.Exit.TimeZone
		}
		email := p.Email
		switch {
		case p.Unavailable:
			email += " (unavailable)"
		case email == "":
			email = "-"
		}
		fmt.Fprintln(tw, strings.Join([]string{strconv.Itoa(i + 1), p.ID, p.Name, email, node, ip, tz}, "\t"))
	}
	tw.Flush()
	return 0
}

func runRCBLogout(*invocation) int { return fail(rcb.Remove()) }

// openBrowser tries to show link in this machine's browser; failure is fine.
func openBrowser(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", link)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}
