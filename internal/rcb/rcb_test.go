package rcb

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var sample = []Profile{
	{ID: "p-0000000001", Name: "Work", Email: "a@example.com", SignedIn: true, Node: &Node{ID: "proxy-a", Name: "LA"}},
	{ID: "p-0000000002", Name: "默认浏览器"},
	{ID: "p-0000000003", Name: "默认浏览器", Email: "b@example.com"},
}

func TestFind(t *testing.T) {
	for sel, want := range map[string]string{
		"p-0000000002":  "p-0000000002",
		"A@Example.com": "p-0000000001",
		"work":          "p-0000000001",
		"b@example.com": "p-0000000003",
	} {
		p, err := Find(sample, sel)
		if err != nil || p.ID != want {
			t.Errorf("Find(%q) = %s, %v; want %s", sel, p.ID, err, want)
		}
	}
	if _, err := Find(sample, "默认浏览器"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Errorf("ambiguous name: %v", err)
	}
	if _, err := Find(sample, "nope"); err == nil || !strings.Contains(err.Error(), "p-0000000001") {
		t.Errorf("unknown name should list profiles: %v", err)
	}
}

func TestUsable(t *testing.T) {
	if sample[0].Usable() != nil {
		t.Error("bound profile not usable")
	}
	if sample[1].Usable() == nil {
		t.Error("profile without node usable")
	}
	if (Profile{Name: "x", Node: &Node{}, Unavailable: true}).Usable() == nil {
		t.Error("unavailable account usable")
	}
	if sample[0].Account() != "rcb-p-0000000001" {
		t.Error(sample[0].Account())
	}
}

func TestProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/remote-cc/v1/profiles" || r.Header.Get("Authorization") != "Bearer rcck_good" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"profiles":[{"id":"p-0000000001","name":"Work","email":"a@example.com","signedIn":true,"unavailable":false,
			"node":{"id":"proxy-a","name":"LA"},"exit":{"ip":"203.0.113.7","city":"Los Angeles","country":"United States","timeZone":"America/Los_Angeles","checkedAt":5}}]}`))
	}))
	defer srv.Close()
	ps, err := Profiles(Link{URL: srv.URL, Token: "rcck_good"})
	if err != nil || len(ps) != 1 || ps[0].Exit.TimeZone != "America/Los_Angeles" || ps[0].Node.ID != "proxy-a" {
		t.Fatalf("%+v %v", ps, err)
	}
	if _, err := Profiles(Link{URL: srv.URL, Token: "rcck_bad"}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("bad token: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://b.example.com/":    "https://b.example.com",
		" https://b.example.com/x/": "https://b.example.com/x",
		"http://localhost:8080/":    "http://localhost:8080",
		"http://127.0.0.2:8080":     "http://127.0.0.2:8080",
		"http://[::1]:8080":         "http://[::1]:8080",
	} {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	// The token must not travel in the clear.
	for _, bad := range []string{"b.example.com", "ftp://x", "", "http://b.example.com", "http://10.0.0.1:8080", "http://localhost.evil.com"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestFindByNumberAndPart(t *testing.T) {
	ps := []Profile{
		{ID: "p-9e4b875d68", Name: "Work", Email: "alice@example.com"},
		{ID: "p-c94214f70d", Name: "Personal", Email: "alice.home@example.com"},
		{ID: "p-0000000003", Name: "work-old", Email: "old@example.com"},
	}
	for sel, want := range map[string]string{
		"2": "p-c94214f70d", "9e4b": "p-9e4b875d68", "PERSONAL": "p-c94214f70d", "old@": "p-0000000003",
		"Work": "p-9e4b875d68", // an exact name wins over "work-old" containing it
	} {
		p, err := Find(ps, sel)
		if err != nil || p.ID != want {
			t.Errorf("%q: %s %v", sel, p.ID, err)
		}
	}
	var amb *AmbiguousError
	if _, err := Find(ps, "alice"); !errors.As(err, &amb) || len(amb.Candidates) != 2 {
		t.Errorf("alice: %v", err)
	}
	if _, err := Find(ps, "nobody"); err == nil || errors.As(err, &amb) {
		t.Errorf("nobody: %v", err)
	}
	if _, err := Find(ps, "4"); err == nil {
		t.Error("4 of 3 profiles was accepted")
	}
}
