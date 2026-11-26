package shim

import (
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"

	"remote-cc/internal/proto"
)

// EnvRealRg is B's own ripgrep on PATH (may be empty), for searches that do
// not run on A.
const EnvRealRg = "RCC_REAL_RG"

// Rg is the B-side `rg` that Claude Code runs for Grep, Glob and its file
// index (USE_BUILTIN_RIPGREP=0 makes it take rg from PATH, see launch.sh).
// A search of the project runs on A, where the files are: a walk through
// sshfs costs a round trip per directory and file. The paths are the same on
// both sides, so the output is the same. Anything else (paths outside the
// project, stdin, a pattern file, a preprocessor), and every search when A
// has no rg or cannot be reached, runs B's ripgrep with the same arguments.
func Rg(args []string) int {
	cwd, _ := os.Getwd()
	if dir, paths, ok := rgTarget(cwd, os.Getenv(EnvRoot), args); ok && !rgMissingOnA() {
		if code, done := rgOnA(dir, args, paths); done {
			return code
		}
	}
	return execLocalRg(args)
}

// Options whose value is a separate argument (unless written --opt=value,
// or attached to a short option: -C3).
var rgValueOpts = map[string]bool{
	"--regexp": true, "--file": true, "--pre": true, "--pre-glob": true,
	"--dfa-size-limit": true, "--encoding": true, "--engine": true,
	"--max-count": true, "--regex-size-limit": true, "--threads": true,
	"--glob": true, "--iglob": true, "--ignore-file": true, "--max-depth": true,
	"--maxdepth": true, "--max-filesize": true, "--type": true, "--type-not": true,
	"--type-add": true, "--type-clear": true, "--after-context": true,
	"--before-context": true, "--color": true, "--colors": true, "--context": true,
	"--context-separator": true, "--field-context-separator": true,
	"--field-match-separator": true, "--hostname-bin": true,
	"--hyperlink-format": true, "--max-columns": true, "--path-separator": true,
	"--replace": true, "--sort": true, "--sortr": true, "--generate": true,
}

const rgShortValue = "efEmjgdtTABCMr"

// Options that name files or programs of the machine rg runs on: such a
// search stays on B, where Claude Code meant them.
var rgHereOnly = map[string]bool{
	"--file": true, "-f": true, "--pre": true, "--ignore-file": true, "--hostname-bin": true,
}

// Options after which rg takes no pattern (the positionals are all paths).
var rgNoPattern = map[string]bool{"--files": true}

// Options that make rg do something other than search: stay on B.
var rgNoSearch = map[string]bool{
	"--type-list": true, "--version": true, "-V": true, "--help": true, "-h": true,
	"--generate": true, "--pcre2-version": true,
}

// rgTarget decides whether a search can run on A: every path it searches is
// inside root and nothing in it refers to B. It returns the directory to run
// in (cwd, or root when cwd is outside it and all paths are absolute) and
// the indexes in args of the path arguments.
func rgTarget(cwd, root string, args []string) (dir string, paths []int, ok bool) {
	if root == "" {
		return "", nil, false
	}
	var pos []int
	pattern := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for j := i + 1; j < len(args); j++ {
				pos = append(pos, j)
			}
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a, "=")
			if rgHereOnly[name] || rgNoSearch[name] {
				return "", nil, false
			}
			if name == "--regexp" {
				pattern = false
			}
			if rgNoPattern[name] {
				pattern = false
			}
			if rgValueOpts[name] && !hasValue {
				i++
			}
		case len(a) > 1 && a[0] == '-':
			for k := 1; k < len(a); k++ {
				c := a[k]
				opt := "-" + string(c)
				if rgHereOnly[opt] || rgNoSearch[opt] {
					return "", nil, false
				}
				if c == 'e' {
					pattern = false
				}
				if strings.IndexByte(rgShortValue, c) >= 0 {
					if k == len(a)-1 {
						i++
					}
					break
				}
			}
		default:
			pos = append(pos, i)
		}
	}
	if pattern && len(pos) > 0 {
		pos = pos[1:]
	}
	if len(pos) == 0 {
		return "", nil, false // rg would read stdin or search cwd: leave it to B
	}
	allAbs := true
	for _, i := range pos {
		p := args[i]
		if p == "-" {
			return "", nil, false
		}
		if !path.IsAbs(p) {
			allAbs = false
		}
		if !inside(abs(cwd, p), root) {
			return "", nil, false
		}
	}
	switch {
	case inside(cwd, root):
		dir = path.Clean(cwd)
	case allAbs:
		dir = path.Clean(root)
	default:
		return "", nil, false
	}
	return dir, pos, true
}

// rgMissingFile marks, for the rest of the session, that A has no rg: the
// run directory holds the command channel's socket.
func rgMissingFile() string {
	addr := os.Getenv(EnvSock)
	if strings.HasPrefix(addr, "tcp:") || addr == "" {
		return ""
	}
	return path.Join(path.Dir(strings.TrimPrefix(addr, "unix:")), "rg-missing")
}

func rgMissingOnA() bool {
	f := rgMissingFile()
	if f == "" {
		return false
	}
	_, err := os.Stat(f)
	return err == nil
}

// rgOnA runs the search on A. done is false when it did not start there
// (A unreachable, no rg there, an older agent): nothing was written then,
// and the caller runs it on B instead.
func rgOnA(dir string, args []string, paths []int) (code int, done bool) {
	addr := os.Getenv(EnvSock)
	if addr == "" {
		return 0, false
	}
	nc, err := dial(addr)
	if err != nil {
		return 0, false
	}
	defer nc.Close()
	c := proto.NewConn(nc)
	err = c.WriteJSON(proto.TStart, proto.Start{
		Version: proto.Version,
		Token:   os.Getenv(EnvToken),
		Cwd:     dir,
		Argv:    append([]string{"rg"}, args...),
		Paths:   paths,
	})
	if err != nil {
		return 0, false
	}
	// rg reads no stdin when given paths.
	c.Write(proto.TStdinEOF, nil)

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			c.WriteJSON(proto.TSignal, proto.Signal{Name: sigName(s)})
		}
	}()

	wrote := false
	for {
		f, err := c.Read()
		if err != nil {
			if !wrote {
				return 0, false
			}
			return fail("connection to local machine lost: %v", err), true
		}
		switch f.Type {
		case proto.TStdout:
			wrote = true
			os.Stdout.Write(f.Payload)
		case proto.TStderr:
			wrote = true
			os.Stderr.Write(f.Payload)
		case proto.TError:
			if wrote {
				return fail("%s", f.Payload), true
			}
			msg := string(f.Payload)
			if strings.Contains(msg, proto.RgMissing) || strings.Contains(msg, `cannot run "rg"`) {
				if mf := rgMissingFile(); mf != "" {
					os.WriteFile(mf, nil, 0o600)
				}
			}
			return 0, false
		case proto.TExit:
			var ex proto.Exit
			if err := json.Unmarshal(f.Payload, &ex); err != nil {
				return fail("bad exit frame: %v", err), true
			}
			return ex.Code, true
		}
	}
}

// execLocalRg replaces this process with B's ripgrep: the one built into
// Claude Code when it is the native build (our parent; argv0 "rg" selects
// it, as Claude Code itself runs it), else RCC_REAL_RG.
func execLocalRg(args []string) int {
	if exe := parentBunExe(); exe != "" {
		err := syscall.Exec(exe, append([]string{"rg", "--no-config"}, args...), os.Environ())
		return fail("exec %s: %v", exe, err)
	}
	if real := os.Getenv(EnvRealRg); real != "" {
		err := syscall.Exec(real, append([]string{"rg"}, args...), os.Environ())
		return fail("exec %s: %v", real, err)
	}
	fmt.Fprintln(os.Stderr, "rg: not installed on this machine")
	return 127
}

// parentBunExe is the parent process's executable (as /proc/PID/exe, which
// works even when an update replaced the file) when it is a Bun standalone
// build, as Claude Code's native binary is (an ELF file with a .bun
// section), else "".
func parentBunExe() string {
	exe := fmt.Sprintf("/proc/%d/exe", os.Getppid())
	f, err := elf.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	if f.Section(".bun") == nil {
		return ""
	}
	return exe
}
