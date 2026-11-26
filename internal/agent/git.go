package agent

import (
	"bytes"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// gitCommand builds the command for a forwarded git call (proto.Start.Argv).
// Paths are the same on both sides except on Windows, where B's /c/… paths
// in the arguments become C:/… (localArgs) and git's output is mapped back
// by driveWriter. git.exe runs directly, not through Git Bash, whose own
// argument conversion would also rewrite values such as core.hooksPath=/dev/null.
func gitCommand(args []string) (*exec.Cmd, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		args = localArgs(args)
	}
	return exec.Command(git, args...), nil
}

// drivePathRe matches B's form of a Windows drive path: /c or /c/….
var drivePathRe = regexp.MustCompile(`^/([A-Za-z])(/.*)?$`)

// localArgs maps arguments that are B-form drive paths ("/c/x", or
// "--opt=/c/x") to C:/x. Everything else passes unchanged.
func localArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if m := drivePathRe.FindStringSubmatch(a); m != nil {
			out[i] = driveLocal(m)
		} else if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
			if m := drivePathRe.FindStringSubmatch(v); m != nil {
				out[i] = k + "=" + driveLocal(m)
			}
		}
	}
	return out
}

func driveLocal(m []string) string {
	rest := m[2]
	if rest == "" {
		rest = "/"
	}
	return strings.ToUpper(m[1]) + ":" + rest
}

// driveRe matches a Windows drive path at the start of a word: "C:/x".
var driveRe = regexp.MustCompile(`(^|[\s"'=(\x00])([A-Za-z]):/`)

// toPosixDrives rewrites C:/… to /c/… (B's form of A's paths, see pathmap).
func toPosixDrives(b []byte) []byte {
	return driveRe.ReplaceAllFunc(b, func(m []byte) []byte {
		pre := m[:len(m)-3]
		return append(append(append([]byte{}, pre...), '/', m[len(m)-3]|0x20), '/')
	})
}

// driveWriter applies toPosixDrives line by line (NUL counts as a line end,
// for -z output) and passes the rest through. json also turns the \ in rg
// --json's path texts into /.
type driveWriter struct {
	w    io.Writer
	buf  []byte
	json bool
}

func (d *driveWriter) convert(b []byte) []byte {
	if d.json {
		b = rgJSONPaths(b)
	}
	return toPosixDrives(b)
}

func (d *driveWriter) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	if i := bytes.LastIndexAny(d.buf, "\n\x00"); i >= 0 {
		if _, err := d.w.Write(d.convert(d.buf[:i+1])); err != nil {
			return 0, err
		}
		d.buf = append(d.buf[:0], d.buf[i+1:]...)
	}
	if len(d.buf) > 1<<16 { // a huge line: give up mapping it
		if _, err := d.w.Write(d.buf); err != nil {
			return 0, err
		}
		d.buf = d.buf[:0]
	}
	return len(p), nil
}

// Flush writes what is left after the command ends.
func (d *driveWriter) Flush() error {
	if len(d.buf) == 0 {
		return nil
	}
	_, err := d.w.Write(d.convert(d.buf))
	d.buf = nil
	return err
}
