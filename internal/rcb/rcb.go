// Package rcb links remote-cc on this machine to a remote-claude-browser
// workbench: it reads the user's browser profiles (Claude account, proxy
// node, exit, time zone) with a client token, so a session can run as that
// profile's Claude identity through that profile's node.
package rcb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Link is what `rcb login` stores on this machine.
type Link struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// Exit is a profile's last checked exit.
type Exit struct {
	IP        string `json:"ip"`
	City      string `json:"city"`
	Country   string `json:"country"`
	TimeZone  string `json:"timeZone"`
	CheckedAt int64  `json:"checkedAt"`
}

// Node is the proxy node a profile is bound to.
type Node struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Profile is one browser profile as the workbench reports it to remote-cc.
type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	SignedIn    bool   `json:"signedIn"`
	Unavailable bool   `json:"unavailable"`
	Node        *Node  `json:"node"`
	Exit        *Exit  `json:"exit"`
}

// Label names the profile for messages.
func (p Profile) Label() string {
	if p.Email != "" && p.Email != p.Name {
		return fmt.Sprintf("%s (%s)", p.Name, p.Email)
	}
	return p.Name
}

// Account is the remote-cc account (Claude config dir) of this profile.
func (p Profile) Account() string { return "rcb-" + p.ID }

// Usable reports why a session cannot run as this profile, or nil.
func (p Profile) Usable() error {
	switch {
	case p.Unavailable:
		return fmt.Errorf("profile %s: the Claude site rejected this account", p.Label())
	case p.Node == nil:
		return fmt.Errorf("profile %s has no proxy node; choose one in the workbench (⋯ → 设置出口)", p.Label())
	}
	return nil
}

func linkPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rcc", "rcb.json"), nil
}

// ErrNotLinked means `rcb login` has not been run on this machine.
var ErrNotLinked = errors.New("this machine is not linked to remote-claude-browser; run: rcb login URL")

// Load reads the stored link.
func Load() (Link, error) {
	p, err := linkPath()
	if err != nil {
		return Link{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Link{}, ErrNotLinked
	}
	if err != nil {
		return Link{}, err
	}
	var l Link
	if err := json.Unmarshal(b, &l); err != nil || l.URL == "" || l.Token == "" {
		return Link{}, fmt.Errorf("%s is damaged; run rcb login again", p)
	}
	return l, nil
}

// Save stores the link, readable only by this user.
func Save(l Link) error {
	p, err := linkPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Remove deletes the stored link.
func Remove() error {
	p, err := linkPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// NormalizeURL checks and trims a workbench address.
func NormalizeURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("give the remote-claude-browser address, e.g. https://browser.example.com")
	}
	u.Path, u.RawQuery, u.Fragment = strings.TrimRight(u.Path, "/"), "", ""
	return u.String(), nil
}

var client = &http.Client{Timeout: 20 * time.Second}

// Profiles lists the profiles the link's user owns.
func Profiles(l Link) ([]Profile, error) {
	req, err := http.NewRequest("GET", l.URL+"/api/remote-cc/v1/profiles", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote-claude-browser at %s: %w", l.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("remote-claude-browser rejected the token (revoked, or its user is disabled); run rcb login again")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("remote-claude-browser answered HTTP %d", resp.StatusCode)
	}
	var v struct{ Profiles []Profile }
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("unexpected answer from %s (is this a remote-claude-browser with remote-cc support?)", l.URL)
	}
	return v.Profiles, nil
}

// Find picks one profile by id, Claude email or name (case-insensitive for
// email and name); an ambiguous or unknown selector lists the candidates.
func Find(ps []Profile, sel string) (Profile, error) {
	sel = strings.TrimSpace(sel)
	for _, p := range ps {
		if p.ID == sel {
			return p, nil
		}
	}
	match := func(f func(Profile) string) []Profile {
		var out []Profile
		for _, p := range ps {
			if v := f(p); v != "" && strings.EqualFold(v, sel) {
				out = append(out, p)
			}
		}
		return out
	}
	for _, f := range []func(Profile) string{
		func(p Profile) string { return p.Email },
		func(p Profile) string { return p.Name },
	} {
		switch m := match(f); len(m) {
		case 0:
			continue
		case 1:
			return m[0], nil
		default:
			return Profile{}, fmt.Errorf("%q matches several profiles; use the id:\n%s", sel, list(m))
		}
	}
	return Profile{}, fmt.Errorf("no profile %q; yours are:\n%s", sel, list(ps))
}

func list(ps []Profile) string {
	var b strings.Builder
	for _, p := range ps {
		node := "no node"
		if p.Node != nil {
			node = p.Node.Name
		}
		fmt.Fprintf(&b, "  %s  %s  [%s]\n", p.ID, p.Label(), node)
	}
	return strings.TrimRight(b.String(), "\n")
}
