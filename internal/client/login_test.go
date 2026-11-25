package client

import (
	"errors"
	"strings"
	"testing"
	"time"

	"remote-cc/internal/rcb"
)

func findLink(out string) (string, bool) { return extractLink(ansi.ReplaceAllString(out, "")) }

func TestExtractLink(t *testing.T) {
	const old = "https://claude.ai/oauth/authorize?code=true&client_id=9d1c&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&code_challenge=abc&state=st4te"
	// What current Claude Code prints.
	const cai = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&code_challenge=abc&state=st4te"
	for _, link := range []string{old, cai} {
		cases := map[string]string{
			"plain":   "Browser didn't open? Use the url below to sign in:\r\n\r\n" + link + "\r\n\r\nPaste code here if prompted > ",
			"ansi":    "\x1b[2K\x1b[1G\x1b[36m" + link + "\x1b[39m\r\n\r\n Paste code here > ",
			"wrapped": "Use the url below:\r\n\r\n" + link[:60] + "\r\n" + link[60:120] + "\r\n" + link[120:] + "\r\n\r\nPaste code > ",
			// Wrapped inside the host/path part, with colour codes around the pieces.
			"wrapped early": "Use the url below:\r\n\r\n\x1b[36m" + link[:25] + "\x1b[39m\r\n\x1b[36m" + link[25:] + "\x1b[39m\r\n\r\nPaste code > ",
			// Another link before the sign-in link.
			"other link first": "See https://docs.anthropic.com/x for help.\r\n\r\n" + link + "\r\n\r\nPaste code > ",
		}
		for name, out := range cases {
			got, ok := findLink(out)
			if !ok || got != link {
				t.Errorf("%s (%s): got %q %v", name, link[8:20], got, ok)
			}
		}
		// Incomplete output: wait for more.
		if _, ok := findLink("sign in:\r\n" + link[:80]); ok {
			t.Error("partial link taken as complete")
		}
	}
	if _, ok := findLink("no link here\r\n"); ok {
		t.Error("found a link in nothing")
	}
	for _, bad := range []string{
		"https://evil.example/oauth/authorize?state=x \r\n",
		"https://claude.com.evil.example/oauth/authorize?state=x \r\n",
		"https://claude.com/oauth/token?state=x \r\n",
	} {
		if l, ok := findLink(bad); ok {
			t.Errorf("took %q", l)
		}
	}
}

func TestLinkWatch(t *testing.T) {
	const link = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&state=st4te"
	var found string
	var missed bool
	w := &linkWatch{found: func(u string) { found = u }, missed: func() { missed = true }}
	// Arrives in pieces, cut in the middle of an escape sequence.
	out := "\x1b[2KBrowser didn't open? Use the url below to sign in:\r\n\r\n\x1b[36m" + link + "\x1b[39m\r\n\r\n Paste code here if prompted > "
	for i := 0; i < len(out); i += 7 {
		w.Write([]byte(out[i:min(i+7, len(out))]))
	}
	if found != link || missed {
		t.Fatalf("found %q missed %v", found, missed)
	}
	// Wrapped with colour codes, every cut point: never a truncated link.
	wrapped := "sign in:\r\n\r\n\x1b[36m" + link[:30] + "\x1b[39m\r\n\x1b[36m" + link[30:] + "\x1b[39m\r\n\r\nPaste code > "
	for cut := 1; cut < len(wrapped); cut++ {
		found = ""
		w = &linkWatch{found: func(u string) { found = u }}
		w.Write([]byte(wrapped[:cut]))
		w.Write([]byte(wrapped[cut:]))
		if found != link {
			t.Fatalf("cut at %d: found %q", cut, found)
		}
	}

	// A sign-in prompt with no link we know: tell the user to paste by hand.
	found, missed = "", false
	w = &linkWatch{found: func(u string) { found = u }, missed: func() { missed = true }}
	w.Write([]byte("Use the url below to sign in:\r\n\r\nhttps://login.example/authorize?state=x\r\n\r\nPaste code here if prompted > "))
	if found != "" || !missed {
		t.Fatalf("unknown link: found %q missed %v", found, missed)
	}
}

func TestTypedText(t *testing.T) {
	for in, want := range map[string]bool{
		"a":            true,
		"\r":           true,
		"\x1b[I":       false, // focus in
		"\x1b[12;40R":  false, // cursor position report
		"\x1b[O\x1b[I": false,
		"\x1b[Ax":      true,
	} {
		if got := typedText([]byte(in)); got != want {
			t.Errorf("typedText(%q) = %v", in, got)
		}
	}
}

func TestWaitForCode(t *testing.T) {
	type reply struct {
		st, code string
		err      error
	}
	run := func(replies ...reply) (string, error, int) {
		n := 0
		code, err := waitForCode(func() (string, string, error) {
			r := replies[min(n, len(replies)-1)]
			n++
			return r.st, r.code, r.err
		}, time.Millisecond, time.Second, nil)
		return code, err, n
	}
	temporary := &rcb.Error{Status: 502}
	// Network trouble and server errors are retried.
	if code, err, n := run(reply{"waiting", "", nil}, reply{"", "", temporary}, reply{"opened", "", nil}, reply{"done", "c#s", nil}); code != "c#s" || err != nil || n != 4 {
		t.Errorf("retry: %q %v %d", code, err, n)
	}
	// Cancelled in the workbench (404): stop and say so.
	if _, err, n := run(reply{"waiting", "", nil}, reply{"", "", rcb.ErrLoginGone}); !errors.Is(err, rcb.ErrLoginGone) || n != 2 {
		t.Errorf("gone: %v %d", err, n)
	}
	// Token revoked (401): stop at once.
	if _, err, n := run(reply{"", "", &rcb.Error{Status: 401}}); err == nil || n != 1 {
		t.Errorf("401: %v %d", err, n)
	}
	// claude ended: stop quietly.
	stop := make(chan struct{})
	close(stop)
	if _, err := waitForCode(func() (string, string, error) { return "waiting", "", nil }, time.Hour, time.Hour, stop); !errors.Is(err, errEnded) {
		t.Errorf("ended: %v", err)
	}
	// Timeout.
	if _, err := waitForCode(func() (string, string, error) { return "", "", temporary }, time.Millisecond, 5*time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("timeout: %v", err)
	}
}
