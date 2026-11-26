package session

import (
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// statusLine is rcc's one line on the terminal while the connection is
// down. Claude Code's interface (Ink) redraws by erasing as many lines as
// it printed last time and printing anew: a line printed below it, or a
// change of the width (lines wrap differently), leaves a copy of the old
// interface on the screen. So the status never prints a newline: it takes
// the cursor's line (below the interface, where Ink leaves the cursor),
// shorter than the width, and is erased before anything else is printed.
// It shows only once an outage has lasted showAfter: short ones leave the
// screen alone.
type statusLine struct {
	mu    sync.Mutex
	out   io.Writer
	width func() int // the terminal's width, 0 if unknown

	msg     string
	showAt  time.Time // zero: not active
	shown   bool      // on the screen now
	touched bool      // was on the screen since begin
	timer   *time.Timer
}

const statusShowAfter = time.Second

// begin starts an outage: set's messages appear from after on.
func (s *statusLine) begin(after time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Still on the screen (a quiet connection, now lost): it stays, and
	// shows the new message at once.
	s.msg, s.touched = "", s.shown
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.shown {
		after = 0
	}
	s.showAt = time.Now().Add(after)
	if after <= 0 {
		return
	}
	s.timer = time.AfterFunc(after, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.showAt.IsZero() && s.msg != "" && !s.shown {
			s.draw()
		}
	})
}

// set changes the message; it is drawn if the outage has lasted long enough.
func (s *statusLine) set(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msg = msg
	if !s.showAt.IsZero() && !time.Now().Before(s.showAt) {
		s.draw()
	}
}

func (s *statusLine) draw() {
	msg := "rcc: " + s.msg
	if w := s.width(); w > 0 {
		msg = truncate(msg, w-1)
	}
	io.WriteString(s.out, "\r"+msg+"\x1b[K")
	s.shown, s.touched = true, true
}

// clear erases the status if it is on the screen; s.mu is held.
func (s *statusLine) clear() {
	if s.shown {
		io.WriteString(s.out, "\r\x1b[K")
		s.shown = false
	}
}

// end ends the outage, erasing the status. It reports whether the status
// was ever on the screen (the line it took may have held part of the
// interface: time for a redraw).
func (s *statusLine) end() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.showAt = time.Time{}
	s.clear()
	return s.touched
}

// final prints a last message on its own line: the session is over.
func (s *statusLine) final(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.showAt = time.Time{}
	s.clear()
	io.WriteString(s.out, "\r\nrcc: "+msg+"\r\n")
}

// writer returns w (the terminal output) behind the status: the status is
// erased before anything is printed.
func (s *statusLine) writer(w io.Writer) io.Writer { return statusWriter{s, w} }

type statusWriter struct {
	s *statusLine
	w io.Writer
}

func (sw statusWriter) Write(p []byte) (int, error) {
	sw.s.mu.Lock()
	defer sw.s.mu.Unlock()
	sw.s.clear()
	return sw.w.Write(p)
}

// truncate cuts s to at most n characters (status messages are ASCII and
// the odd "…").
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
