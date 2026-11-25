package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"remote-cc/internal/procjob"
	"remote-cc/internal/rcb"
	"remote-cc/internal/remote"
)

// rcbLogin runs `claude auth login` for a profile and does the sign-in in
// the profile's browser in remote-claude-browser: the sign-in link claude
// prints goes to the workbench, which opens it there; once the user clicks
// "Authorize", the code comes back and is typed into claude. Pasting the
// code by hand still works.
func (s *session) rcbLogin(script string) (int, error) {
	p, link := s.cfg.RCB, *s.cfg.RCBLink
	fd := int(os.Stdin.Fd())
	cols, rows := 0, 0
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		cols, rows = w, h
	}
	// stdin is a pipe (so the code can be typed in), hence -t twice for a pty.
	c := s.conn.Script(true, []string{"-t"}, remote.Vars("TTY_COLS", fmt.Sprint(cols), "TTY_ROWS", fmt.Sprint(rows))+script)
	in, err := c.StdinPipe()
	if err != nil {
		return 1, err
	}
	// Terminal closed or rcc killed: end ssh so the terminal is restored
	// below instead of being left raw.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	ended := make(chan struct{})
	var typing sync.Mutex
	var linkSeen, typed atomic.Bool
	say := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "\r\n\x1b[2mrcc: "+format+"\x1b[0m\r\n", a...)
	}
	watch := &linkWatch{found: func(url string) {
		linkSeen.Store(true)
		go func() {
			id, status, err := rcb.StartLogin(link, p.ID, url)
			if err != nil {
				say("could not hand the sign-in to remote-claude-browser (%v); open the link above yourself", err)
				return
			}
			if status == "opened" {
				say("the sign-in page is open in profile %s's browser in remote-claude-browser: click Authorize there", p.Name)
			} else {
				say("in remote-claude-browser, open profile %s: a banner at the top offers 打开授权页 (open the sign-in page); click it, then Authorize", p.Name)
			}
			code, err := waitForCode(func() (string, string, error) { return rcb.PollLogin(link, id) }, 1500*time.Millisecond, 15*time.Minute, ended)
			switch {
			case errors.Is(err, errEnded):
			case err != nil:
				say("%v; paste the code here by hand", err)
			default:
				typing.Lock()
				defer typing.Unlock()
				if typed.Load() {
					// Typing it in would mix it with what the user typed.
					say("signed in; paste this code into Claude Code: %s", code)
					return
				}
				// Only the code: claude reads this prompt in raw mode, where a line-kill
				// (^U) would arrive as part of the code. Typed input is handled above.
				io.WriteString(in, code+"\r")
			}
		}()
	}, missed: func() {
		say("could not find the sign-in link in Claude Code's output; open it yourself and paste the code here")
	}}
	c.Stdout = io.MultiWriter(os.Stdout, watch)
	c.Stderr = os.Stderr
	if term.IsTerminal(fd) {
		if old, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, old)
		}
	}
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	if err := c.Start(); err != nil {
		return 1, err
	}
	procjob.TrackProcess(c.Process.Pid) // TERM lets ssh restore the terminal if rcc dies
	go func() {
		select {
		case <-sigs:
			c.Process.Kill()
		case <-ended:
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if linkSeen.Load() && typedText(buf[:n]) {
					typed.Store(true)
				}
				typing.Lock()
				in.Write(buf[:n])
				typing.Unlock()
			}
			if err != nil {
				in.Close()
				return
			}
		}
	}()
	err = c.Wait()
	close(ended)
	return remote.ExitCode(err)
}

var errEnded = errors.New("claude ended")

// waitForCode polls a sign-in until its code arrives. Network trouble and
// server errors are retried; a sign-in the workbench no longer knows (the
// user cancelled it, or it expired), a refused link or the timeout end it.
func waitForCode(poll func() (status, code string, err error), every, timeout time.Duration, stop <-chan struct{}) (string, error) {
	var last error
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); {
		st, code, err := poll()
		switch {
		case errors.Is(err, rcb.ErrLoginGone):
			return "", err
		case rcb.Temporary(err):
			last = err
		case err != nil:
			return "", fmt.Errorf("stopped waiting for the sign-in (%v)", err)
		case st == "done" && code != "":
			return code, nil
		}
		select {
		case <-stop:
			return "", errEnded
		case <-time.After(every):
		}
	}
	if last != nil {
		return "", fmt.Errorf("gave up waiting for the sign-in (%v)", last)
	}
	return "", errors.New("gave up waiting for the sign-in")
}

// typedText reports whether terminal input holds more than the terminal's
// own reports (focus, cursor position, …), i.e. the user typed something.
func typedText(b []byte) bool {
	return ansi.ReplaceAllString(string(b), "") != ""
}

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	// Current Claude Code signs in at claude.com/cai/oauth/authorize; allow
	// such path prefixes on all the hosts it has used.
	signInLink = regexp.MustCompile(`^https://(claude\.ai|claude\.com|platform\.claude\.com|console\.anthropic\.com)(/[A-Za-z0-9._~-]+)*/oauth/authorize\?`)
	// An escape sequence cut off at the end of the output so far.
	partialEsc = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*|\][^\x07\x1b]*\x1b?)?$`)
	// Claude Code's prompt for the code, shown after the link.
	codePrompt = regexp.MustCompile(`(?i)paste\s+(the\s+)?code`)
)

// linkWatch finds Claude Code's sign-in link in its terminal output, also
// when the terminal wrapped it over several lines. missed is called instead
// when Claude asks for the code without a link found.
type linkWatch struct {
	mu     sync.Mutex
	buf    strings.Builder
	done   bool
	found  func(string)
	missed func()
}

func (w *linkWatch) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return len(b), nil
	}
	w.buf.Write(b)
	if w.buf.Len() > 1<<20 {
		w.done = true
		return len(b), nil
	}
	if partialEsc.MatchString(w.buf.String()) {
		return len(b), nil // its rest would end a link early
	}
	text := ansi.ReplaceAllString(w.buf.String(), "")
	if url, ok := extractLink(text); ok {
		w.done = true
		w.found(url)
	} else if codePrompt.MatchString(text) {
		w.done = true
		if w.missed != nil {
			w.missed()
		}
	}
	return len(b), nil
}

// extractLink returns the first complete sign-in link in terminal output
// (ANSI codes removed). Wrapped lines are joined; the link counts as
// complete once it carries a state and the text after it has moved on.
func extractLink(text string) (string, bool) {
	for off := 0; ; {
		i := strings.Index(text[off:], "https://")
		if i < 0 {
			return "", false
		}
		link, ended := joinLink(text[off+i:])
		if !ended {
			return "", false // more may come
		}
		if signInLink.MatchString(link) && strings.Contains(link, "state=") {
			return link, true
		}
		off += i + len("https://")
	}
}

// joinLink reads the URL at the start of text across wrapped lines; ended
// is false while the output may still continue it.
func joinLink(text string) (link string, ended bool) {
	var b strings.Builder
	for i := 0; i < len(text); {
		ch := text[i]
		switch {
		case ch == '\r' || ch == '\n':
			// A wrapped line continues after the line break (and any indentation).
			j := i
			for j < len(text) && (text[j] == '\r' || text[j] == '\n' || text[j] == ' ') {
				j++
			}
			if j >= len(text) {
				return "", false
			}
			if !urlChar(text[j]) || strings.Count(text[i:j], "\n") > 1 {
				return b.String(), true
			}
			i = j
		case urlChar(ch):
			b.WriteByte(ch)
			i++
		default:
			return b.String(), true
		}
	}
	return "", false
}

func urlChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~:/?#[]@!$&'()*+,;=%", c) >= 0
}
