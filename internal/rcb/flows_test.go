package rcb

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConnectWaitsForApproval(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/remote-cc/v1/devices":
			var in struct{ Kind, Name string }
			json.NewDecoder(r.Body).Decode(&in)
			if in.Kind != "client" || in.Name != "laptop" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"deviceCode":"dc","userCode":"ABCD-EFGH","verifyUrl":"https://rcb/?remote-cc=ABCD-EFGH","interval":1,"expiresIn":30}`))
		case "/api/remote-cc/v1/devices/token":
			if polls.Add(1) < 2 {
				w.WriteHeader(202)
				w.Write([]byte(`{"status":"pending"}`))
				return
			}
			w.Write([]byte(`{"status":"approved","token":"rcck_x"}`))
		}
	}))
	defer srv.Close()
	var shown string
	tok, err := Connect(srv.URL, "client", "laptop", func(code, link string) { shown = code + " " + link })
	if err != nil || tok != "rcck_x" || !strings.Contains(shown, "ABCD-EFGH") {
		t.Fatalf("%q %v %q", tok, err, shown)
	}
}

func TestConnectDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/remote-cc/v1/devices" {
			w.Write([]byte(`{"deviceCode":"dc","userCode":"ABCD-EFGH","verifyUrl":"x","interval":1,"expiresIn":30}`))
			return
		}
		w.WriteHeader(410)
		w.Write([]byte(`{"error":"REMOTE_CC_DEVICE_DENIED","message":"连接请求被拒绝"}`))
	}))
	defer srv.Close()
	if _, err := Connect(srv.URL, "client", "laptop", func(string, string) {}); err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("got %v", err)
	}
}

func TestRememberLookup(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg) // macOS UserConfigDir
	proj := filepath.Join(cfg, "proj")
	if err := Remember(proj, Use{Profile: "p-1", Server: "s-cc"}); err != nil {
		t.Fatal(err)
	}
	if u, dir, ok := Lookup(filepath.Join(proj, "src", "pkg")); !ok || u.Profile != "p-1" || dir != proj {
		t.Errorf("subdirectory: %+v %q %v", u, dir, ok)
	}
	if _, _, ok := Lookup(cfg); ok {
		t.Error("parent directory matched")
	}
	b, _ := os.ReadFile(filepath.Join(cfg, "rcc", "rcb-dirs.json"))
	if !strings.Contains(string(b), "s-cc") {
		t.Errorf("stored: %s", b)
	}
}

func TestServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh", "remote-cc"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "remote-cc", "config"), []byte(`# remote-cc:a begin
Host a-cc-gw
    HostName a.example
Host a-cc
    HostName rcc-owner
Host b-cc
`), 0o600)
	if got := strings.Join(Servers(), ","); got != "a-cc,b-cc" {
		t.Errorf("got %s", got)
	}
}

func TestPollLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/remote-cc/v1/logins/done":
			w.Write([]byte(`{"status":"done","code":"c0de#st4te"}`))
		case "/api/remote-cc/v1/logins/gone":
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"REMOTE_CC_LOGIN_UNKNOWN","message":"登录请求已过期，请重新运行 login"}`))
		case "/api/remote-cc/v1/logins/busy":
			w.WriteHeader(502)
		default:
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	l := Link{URL: srv.URL, Token: "rcck_x"}
	if st, code, err := PollLogin(l, "done"); err != nil || st != "done" || code != "c0de#st4te" {
		t.Errorf("done: %q %q %v", st, code, err)
	}
	// Cancelled in the workbench, or expired.
	if _, _, err := PollLogin(l, "gone"); !errors.Is(err, ErrLoginGone) || Temporary(err) {
		t.Errorf("gone: %v", err)
	}
	if _, _, err := PollLogin(l, "busy"); err == nil || !Temporary(err) {
		t.Errorf("5xx should be temporary: %v", err)
	}
	if _, _, err := PollLogin(l, "revoked"); err == nil || Temporary(err) || errors.Is(err, ErrLoginGone) {
		t.Errorf("401: %v", err)
	}
	srv.Close()
	if _, _, err := PollLogin(l, "done"); err == nil || !Temporary(err) {
		t.Errorf("unreachable should be temporary: %v", err)
	}
}
