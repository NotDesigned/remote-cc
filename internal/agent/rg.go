package agent

import (
	"bytes"
	"os/exec"
	"regexp"
	"runtime"
)

// rgCommand builds the command for a forwarded search (proto.Start.Argv
// ["rg", …], see shim.Rg). --no-config keeps this machine's
// RIPGREP_CONFIG_PATH from changing the output Claude Code parses (its own
// built-in rg runs the same way). On Windows the path arguments (paths, the
// indexes of those in args) become C:/… and rg prints / between path parts,
// so that driveWriter can map the output back to B's /c/… form.
func rgCommand(args []string, paths []int) (*exec.Cmd, error) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		return nil, err
	}
	pre := []string{"--no-config"}
	if runtime.GOOS == "windows" {
		args = rgLocalArgs(args, paths)
		pre = append(pre, "--path-separator=/")
	}
	return exec.Command(rg, append(pre, args...)...), nil
}

// rgPathTextRe matches a path in rg --json output: "path":{"text":"…"}.
var rgPathTextRe = regexp.MustCompile(`"path":\{"text":"(?:[^"\\]|\\.)*"`)

// rgJSONPaths turns the separators in rg --json's path texts into /:
// --path-separator does not apply to --json, which writes C:\\x\\y (a
// Windows path has no \ of its own, so every escaped \ is a separator).
func rgJSONPaths(b []byte) []byte {
	if !bytes.Contains(b, []byte(`"path":{"text":"`)) {
		return b
	}
	return rgPathTextRe.ReplaceAllFunc(b, func(m []byte) []byte {
		return bytes.ReplaceAll(m, []byte(`\\`), []byte("/"))
	})
}

// rgLocalArgs maps the path arguments that are B-form drive paths.
func rgLocalArgs(args []string, paths []int) []string {
	out := append([]string(nil), args...)
	for _, i := range paths {
		if i >= 0 && i < len(out) {
			if m := drivePathRe.FindStringSubmatch(out[i]); m != nil {
				out[i] = driveLocal(m)
			}
		}
	}
	return out
}
