// Package session runs a Claude Code session so that it outlives the network
// connection it was started over.
//
// On the server a daemon (rcc-shim __rcc-daemon) holds everything a session
// needs: Claude Code in a pseudo-terminal, the sshfs mount of the project
// directory and the socket rcc-shim reaches for commands. It is detached from
// ssh, so a dropped connection does not end it. Here (internal/client), rcc
// reaches the daemon through `ssh … rcc-shim __rcc-dial`, which only splices
// ssh's stdio to the daemon's socket.
//
// Over that connection both sides run a resumable stream (internal/resume)
// and, on top of it, yamux streams:
//
//	T  terminal: Claude Code's pseudo-terminal ↔ this terminal (opened here)
//	F  files: sshfs on the server ↔ the SFTP server here (opened here)
//	…  one stream per command rcc-shim runs (opened by the daemon)
//
// When the connection drops, rcc connects again and both sides resume where
// they were: commands keep running, file operations wait, the terminal
// output arrives. The daemon gives up after the grace period.
package session

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"remote-cc/internal/proto"
)

// Stream kinds: the first byte of a stream opened here.
const (
	kindTerminal = 'T'
	kindFiles    = 'F'
)

// Frames on the terminal stream (proto framing).
const (
	tData   proto.FrameType = 1 // raw bytes, both ways
	tOpen   proto.FrameType = 2 // JSON Open, here → daemon, first frame
	tResize proto.FrameType = 3 // JSON Size, here → daemon
	tHangup proto.FrameType = 4 // here → daemon: end Claude Code (SIGHUP)
	tExit   proto.FrameType = 5 // JSON Exit, daemon → here
	tError  proto.FrameType = 6 // message, daemon → here: the session could not start
)

// Open starts the terminal: its size and the environment Claude Code gets
// from this terminal (TERM, locale).
type Open struct {
	Size
	Env map[string]string `json:"env,omitempty"`
}

type Size struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type Exit struct {
	Code int `json:"code"`
}

// TerminalEnv lists the variables passed from this terminal to Claude Code,
// as ssh -t would (TERM) plus what a SendEnv setup usually adds.
var TerminalEnv = []string{"TERM", "COLORTERM", "LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TERM_PROGRAM"}

// DefaultGrace is how long the daemon keeps a session without a connection.
const DefaultGrace = 10 * time.Minute

const helloPrefix = "rcc-session 1 "

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// The resumable stream underneath never fails while the network is
	// away, it only waits; yamux must wait with it.
	c.EnableKeepAlive = false
	c.ConnectionWriteTimeout = 365 * 24 * time.Hour
	c.StreamOpenTimeout = 0
	c.StreamCloseTimeout = 10 * time.Minute
	c.MaxStreamWindowSize = 1 << 20 // high-latency links: larger windows
	c.LogOutput = io.Discard
	return c
}

// sayHello is the client's first line on a new connection.
func sayHello(rw io.ReadWriter, token string) error {
	if _, err := io.WriteString(rw, helloPrefix+token+"\n"); err != nil {
		return err
	}
	line, err := readLine(rw, 256)
	if err != nil {
		return fmt.Errorf("no answer from the session on the server: %w", err)
	}
	if line != "ok" {
		return helloError(strings.TrimPrefix(line, "error "))
	}
	return nil
}

// readLine reads one line byte by byte, so nothing after it is consumed
// (the resumable stream starts right after).
func readLine(r io.Reader, max int) (string, error) {
	var b strings.Builder
	var c [1]byte
	for b.Len() < max {
		if _, err := io.ReadFull(r, c[:]); err != nil {
			return "", err
		}
		if c[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c[0])
	}
	return "", errors.New("line too long")
}

// splice copies both ways and closes both when either direction ends.
func splice(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}
