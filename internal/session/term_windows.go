//go:build windows

package session

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

type stdTerminal struct{}

// StdTerminal is this process's console, or nil when stdin or stdout is not
// one.
func StdTerminal() Terminal {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil
	}
	return stdTerminal{}
}

// MakeRaw puts the console in raw mode with virtual terminal input (keys as
// escape sequences) and output (Claude Code's escape sequences rendered), as
// OpenSSH for Windows does.
func (stdTerminal) MakeRaw() (func(), error) {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	out := windows.Handle(os.Stdout.Fd())
	var mode uint32
	restoreOut := func() {}
	if windows.GetConsoleMode(out, &mode) == nil {
		if windows.SetConsoleMode(out, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN) == nil {
			restoreOut = func() { windows.SetConsoleMode(out, mode) }
		}
	}
	return func() {
		restoreOut()
		term.Restore(int(os.Stdin.Fd()), st)
	}, nil
}

func (stdTerminal) Size() (Size, bool) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return Size{}, false
	}
	return Size{Rows: uint16(h), Cols: uint16(w)}, true
}

// Resized polls: Windows has no SIGWINCH.
func (t stdTerminal) Resized() <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		last, _ := t.Size()
		for range time.Tick(300 * time.Millisecond) {
			if s, ok := t.Size(); ok && s != last {
				last = s
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}
