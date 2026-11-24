package shim

import (
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"

	"remote-cc/internal/proto"
)

// Git environment, set by the session's run script on B.
const (
	EnvRoot    = "RCC_ROOT"     // the project mount (same path as on A)
	EnvRealGit = "RCC_REAL_GIT" // B's own git, for everything outside it
)

// Git is the B-side `git` that Claude Code finds first in its PATH. Its own
// git calls (repository detection, status, `--worktree`, …) on the project
// run on A, where the repository really is: A's git, config, credentials and
// hooks, at local speed instead of through sshfs. Calls about anything else
// (plugin marketplaces in ~/.claude, …) go to B's git.
func Git(args []string) int {
	cwd, _ := os.Getwd()
	dir, rest, local := gitTarget(cwd, os.Getenv(EnvRoot), args)
	if !local {
		real := os.Getenv(EnvRealGit)
		if real == "" {
			fmt.Fprintln(os.Stderr, "git: not installed on this machine")
			return 127
		}
		err := syscall.Exec(real, append([]string{"git"}, args...), os.Environ())
		return fail("exec %s: %v", real, err)
	}
	c, done, code := connect()
	if c == nil {
		return code
	}
	defer done()
	return exchange(c, proto.Start{
		Version: proto.Version,
		Token:   os.Getenv(EnvToken),
		Cwd:     dir,
		Env:     gitEnv(os.Environ()),
		Argv:    append([]string{"git"}, rest...),
	}, "")
}

// gitTarget works out the directory a git command acts on (cwd, changed by
// leading -C options) and whether it is inside root. For a forwarded call it
// returns that directory and the arguments without the -C options.
func gitTarget(cwd, root string, args []string) (dir string, rest []string, inRoot bool) {
	dir = cwd
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			if p := args[i+1]; path.IsAbs(p) {
				dir = path.Clean(p)
			} else if p != "" {
				dir = path.Join(dir, p)
			}
			i += 2
			continue
		case a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			// A separate path or value; paths must lie inside root too.
			if i+1 < len(args) && a != "-c" && a != "--namespace" && !inside(abs(dir, args[i+1]), root) {
				return cwd, args, false
			}
			i += 2
			continue
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			_, v, _ := strings.Cut(a, "=")
			if !inside(abs(dir, v), root) {
				return cwd, args, false
			}
		case !strings.HasPrefix(a, "-"):
			// The subcommand: global options end here.
			i = len(args)
			continue
		}
		i++
	}
	if root == "" || !inside(dir, root) {
		return cwd, args, false
	}
	for j := 0; j < len(args); j++ {
		if !strings.HasPrefix(args[j], "-") {
			rest = append(rest, args[j:]...)
			break
		}
		if args[j] == "-C" && j+1 < len(args) {
			j++
			continue
		}
		rest = append(rest, args[j])
		if (args[j] == "-c" || args[j] == "--git-dir" || args[j] == "--work-tree" || args[j] == "--namespace") && j+1 < len(args) {
			j++
			rest = append(rest, args[j])
		}
	}
	return dir, rest, true
}

func abs(dir, p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(dir, p)
}

func inside(p, root string) bool {
	if root == "" {
		return false
	}
	root = path.Clean(root)
	p = path.Clean(p)
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// gitEnv forwards what Claude Code sets for git (GIT_*, locale overrides,
// terminal hints); B's own environment stays on B.
func gitEnv(environ []string) map[string]string {
	out := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "GIT_") && k != "GIT_EXEC_PATH":
			out[k] = v
		case k == "LC_ALL" || k == "LANGUAGE" || k == "TERM" || k == "NO_COLOR" || k == "COLUMNS":
			out[k] = v
		}
	}
	return out
}
