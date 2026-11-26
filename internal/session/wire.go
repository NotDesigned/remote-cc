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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"remote-cc/internal/proto"
	"remote-cc/internal/resume"
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
	tRedraw proto.FrameType = 7 // here → daemon: have Claude Code draw its interface again
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
	// A stream may have this much in flight: 1 MiB capped a stream at about
	// 3.5 MiB/s at 50 ms round trip (a window update reaches the sender
	// only after half the window was read); 4 MiB reaches ssh's own speed.
	c.MaxStreamWindowSize = 4 << 20
	c.LogOutput = io.Discard
	return c
}

// wholeFrames hands yamux's frames to the stream underneath in one Write
// each. yamux writes a data frame's 12-byte header and its body in two
// calls; sent apart, the lone header is a small packet that the next one
// may wait a round trip behind (Nagle), which doubled the latency of every
// keystroke and file request. Only yamux's send loop writes, so there is
// no concurrency here.
type wholeFrames struct {
	io.ReadWriteCloser
	hdr []byte // a data frame's header, held until its body comes
}

const (
	yamuxHeaderSize = 12
	yamuxTypeData   = 0
)

func (w *wholeFrames) Write(p []byte) (int, error) {
	if w.hdr == nil && len(p) == yamuxHeaderSize && p[1] == yamuxTypeData && binary.BigEndian.Uint32(p[8:]) > 0 {
		w.hdr = append([]byte(nil), p...)
		return len(p), nil
	}
	if w.hdr != nil {
		b := make([]byte, 0, len(w.hdr)+len(p))
		b = append(append(b, w.hdr...), p...)
		w.hdr = nil
		if _, err := w.ReadWriteCloser.Write(b); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return w.ReadWriteCloser.Write(p)
}

// attachHello says hello on a new connection and resumes link on it. The
// resume handshake follows the hello without waiting for the answer, a
// round trip less to start and to reconnect: the daemon (and __rcc-dial)
// read the hello line byte by byte, so what follows waits in the
// connection for the daemon's resume handshake. A refusal fails the attach.
func attachHello(link *resume.Conn, tr io.ReadWriteCloser, token string) (<-chan struct{}, error) {
	if _, err := io.WriteString(tr, helloPrefix+token+"\n"); err != nil {
		tr.Close()
		return nil, err
	}
	return link.Attach(&helloReply{ReadWriteCloser: tr})
}

// sayHello says hello on a new connection and waits up to limit for the
// answer: the connection is through to the daemon. The resume handshake is
// next on the returned connection.
func sayHello(tr io.ReadWriteCloser, token string, limit time.Duration) (io.ReadWriteCloser, error) {
	h := &helloReply{ReadWriteCloser: tr}
	t := time.AfterFunc(limit, func() { tr.Close() })
	_, err := io.WriteString(tr, helloPrefix+token+"\n")
	if err == nil {
		err = h.answer()
	}
	t.Stop()
	if err != nil {
		tr.Close()
		return nil, err
	}
	return h, nil
}

// helloReply reads the answer to the hello before the stream.
type helloReply struct {
	io.ReadWriteCloser
	answered bool
	err      error
}

func (h *helloReply) Read(p []byte) (int, error) {
	if err := h.answer(); err != nil {
		return 0, err
	}
	return h.ReadWriteCloser.Read(p)
}

// answer reads the answer to the hello, the first time.
func (h *helloReply) answer() error {
	if !h.answered {
		h.answered = true
		line, err := readLine(h.ReadWriteCloser, 256)
		switch {
		case err != nil:
			h.err = fmt.Errorf("no answer from the session on the server: %w", err)
		case line != "ok":
			h.err = helloError(strings.TrimPrefix(line, "error "))
		}
	}
	return h.err
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
