// Package proto defines the framed wire protocol spoken between rcc-shim (on
// machine B) and the rcc agent (on machine A) over the reverse-forwarded socket.
//
// Each frame is: type (1 byte) | length (4 bytes, big endian) | payload.
// One connection carries exactly one command.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type FrameType byte

const (
	// shim -> agent
	TStart    FrameType = 1 // JSON Start
	TStdin    FrameType = 2 // raw bytes
	TStdinEOF FrameType = 3 // empty
	TSignal   FrameType = 4 // JSON Signal

	// agent -> shim
	TStdout FrameType = 16 // raw bytes
	TStderr FrameType = 17 // raw bytes
	TExit   FrameType = 18 // JSON Exit
	TError  FrameType = 19 // UTF-8 message; agent could not run the command
)

const MaxPayload = 1 << 20

// Version is bumped on incompatible protocol changes.
const Version = 1

type Start struct {
	Version int               `json:"v"`
	Token   string            `json:"token"`
	Cmd     string            `json:"cmd"`
	Cwd     string            `json:"cwd"` // in B's path form
	Env     map[string]string `json:"env,omitempty"`
	// CaptureCwd asks the agent to set $RCC_CWD_FILE to a temp file and
	// return its trimmed contents in Exit.Cwd.
	CaptureCwd bool `json:"captureCwd,omitempty"`
}

// Signal names are portable ("INT", "TERM", "HUP", "KILL").
type Signal struct {
	Name string `json:"name"`
}

type Exit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
}

type Frame struct {
	Type    FrameType
	Payload []byte
}

// Conn wraps a stream with concurrency-safe frame writes.
type Conn struct {
	rw io.ReadWriter
	mu sync.Mutex
}

func NewConn(rw io.ReadWriter) *Conn { return &Conn{rw: rw} }

func (c *Conn) Write(t FrameType, p []byte) error {
	if len(p) > MaxPayload {
		return fmt.Errorf("frame too large: %d", len(p))
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.rw.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.rw.Write(p)
	return err
}

func (c *Conn) WriteJSON(t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(t, b)
}

// Read is not safe for concurrent use; one reader goroutine per Conn.
func (c *Conn) Read() (Frame, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.rw, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return Frame{}, errors.New("frame too large")
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.rw, p); err != nil {
		return Frame{}, err
	}
	return Frame{Type: FrameType(hdr[0]), Payload: p}, nil
}

// ChunkWriter turns an io.Writer stream into frames of type t.
type ChunkWriter struct {
	C *Conn
	T FrameType
}

func (w ChunkWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > 64<<10 {
			n = 64 << 10
		}
		if err := w.C.Write(w.T, p[:n]); err != nil {
			return total - len(p), err
		}
		p = p[n:]
	}
	return total, nil
}
