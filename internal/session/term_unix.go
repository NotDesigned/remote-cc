//go:build !windows

package session

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

type stdTerminal struct{}

// StdTerminal is this process's terminal, or nil when stdin or stdout is
// not one.
func StdTerminal() Terminal {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil
	}
	return stdTerminal{}
}

func (stdTerminal) MakeRaw() (func(), error) {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { term.Restore(int(os.Stdin.Fd()), st) }, nil
}

func (stdTerminal) Size() (Size, bool) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return Size{}, false
	}
	return Size{Rows: uint16(h), Cols: uint16(w)}, true
}

func (stdTerminal) Resized() <-chan struct{} {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	out := make(chan struct{}, 1)
	go func() {
		for range sig {
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out
}
