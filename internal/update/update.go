// Package update replaces the running rcc binary with a GitHub release.
package update

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo can be overridden with $RCC_REPO (owner/name) for forks.
func Repo() string {
	if r := os.Getenv("RCC_REPO"); r != "" {
		return r
	}
	return "NotDesigned/remote-cc"
}

var client = &http.Client{Timeout: 5 * time.Minute}

func get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "remote-cc-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// Latest returns the tag of the latest release.
func Latest() (string, error) {
	resp, err := get("https://api.github.com/repos/" + Repo() + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.TagName == "" {
		return "", fmt.Errorf("could not read the latest release: %v", err)
	}
	return r.TagName, nil
}

// Newer reports whether tag is a newer version than current. Non-release
// builds ("dev", "v0.2.2-3-gabc") count as older than any release.
func Newer(tag, current string) bool {
	t, ok1 := parse(tag)
	c, ok2 := parse(current)
	if !ok1 {
		return false
	}
	if !ok2 {
		return true
	}
	for i := range t {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Asset is the release file name for this platform.
func Asset() string {
	name := "rcc-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Install downloads tag's binary for this platform, verifies it against the
// release's SHA256SUMS and replaces the running executable.
func Install(tag string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	base := "https://github.com/" + Repo() + "/releases/download/" + tag + "/"

	want, err := checksum(base+"SHA256SUMS", Asset())
	if err != nil {
		return "", err
	}
	resp, err := get(base + Asset())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Temp file next to the executable so the final rename is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".rcc-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write next to %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", Asset(), got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// A running .exe cannot be overwritten or deleted, but it can be renamed.
		old := exe + ".old"
		os.Remove(old)
		if _, err := os.Stat(old); err == nil {
			old = fmt.Sprintf("%s.%d.old", exe, time.Now().Unix())
		}
		if err := os.Rename(exe, old); err != nil {
			return "", err
		}
		if err := os.Rename(tmp.Name(), exe); err != nil {
			os.Rename(old, exe)
			return "", err
		}
		return exe, nil
	}
	return exe, os.Rename(tmp.Name(), exe)
}

func checksum(url, asset string) (string, error) {
	resp, err := get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return f[0], nil
		}
	}
	return "", errors.New("no checksum for " + asset + " in SHA256SUMS")
}

// CleanupOld removes binaries left by a previous Windows update.
func CleanupOld() {
	if runtime.GOOS != "windows" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	matches, _ := filepath.Glob(exe + "*.old")
	for _, m := range matches {
		os.Remove(m)
	}
}
