package session

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// endedMessage is what __rcc-dial says when the session is gone; the client
// stops reconnecting when it hears it.
const endedMessage = "the session on the server has ended"

// ErrEnded: the daemon is gone (Claude Code exited, or the grace period ran
// out while the connection was down).
var ErrEnded = errors.New(endedMessage)

// Dial is `rcc-shim __rcc-dial SOCKET`: it connects ssh's stdio to the
// session daemon's socket.
func Dial(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rcc-shim __rcc-dial SOCKET")
		return 2
	}
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ { // the daemon may still be starting
		if c, err = net.Dial("unix", args[0]); err == nil {
			break
		}
		if _, serr := os.Stat(args[0]); errors.Is(serr, os.ErrNotExist) && i > 10 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintf(os.Stdout, "error %s\n", endedMessage)
		return 3
	}
	splice(c, stdio{})
	return 0
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { os.Stdin.Close(); return os.Stdout.Close() }

// helloError turns the daemon's (or __rcc-dial's) refusal into an error.
func helloError(msg string) error {
	if strings.Contains(msg, endedMessage) {
		return ErrEnded
	}
	return errors.New(msg)
}

var _ io.ReadWriteCloser = stdio{}
