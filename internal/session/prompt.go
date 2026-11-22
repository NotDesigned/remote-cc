package session

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// systemPrompt tells Claude where its tools really act. Without it, Claude
// trusts the environment section of its prompt, which describes B. The text
// is deterministic per machine and project so the prompt cache stays warm.
func systemPrompt(remoteDir, shell string) string {
	host, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "# remote-cc\n\n")
	fmt.Fprintf(&b, "Claude Code runs on a remote host, but this session works on the user's own machine through remote-cc.\n\n")
	fmt.Fprintf(&b, "- Every Bash command runs on the user's machine: %s (%s), hostname %q, shell %s. "+
		"Use commands, package managers and path conventions for that system. The platform and OS in your environment "+
		"information describe the remote host, not the machine where commands run.\n", localOS(), runtime.GOARCH, host, shellName(shell))
	fmt.Fprintf(&b, "- %s is the user's local project directory, mounted on the remote host at the same path, so Read, Edit, "+
		"Write, Glob and Grep inside it act on the user's files. File tools on paths outside it act on the remote host; "+
		"use Bash for other locations on the user's machine.\n", remoteDir)
	fmt.Fprintf(&b, "- Network access from Bash (curl, git, package managers) uses the user's machine and network.\n")
	if runtime.GOOS == "windows" {
		fmt.Fprintf(&b, "- The shell is Git Bash on Windows: a path like /c/Users/me is C:\\Users\\me. Windows programs "+
			"(cmd.exe, powershell.exe, any .exe) can be run; Linux system tools such as apt or systemctl are not available. "+
			"Pass Windows-style flags to cmd as `cmd //c ...`.\n")
	}
	return b.String()
}

func localOS() string {
	switch runtime.GOOS {
	case "linux":
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
					return "Linux, " + strings.Trim(v, `"`)
				}
			}
		}
		return "Linux"
	case "darwin":
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			return "macOS " + strings.TrimSpace(string(out))
		}
		return "macOS"
	case "windows":
		if out, err := exec.Command("cmd", "/c", "ver").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
		return "Windows"
	}
	return runtime.GOOS
}

func shellName(path string) string {
	if runtime.GOOS == "windows" {
		return "Git Bash (" + path + ")"
	}
	return path
}

// withSystemPrompt adds our prompt to the claude arguments. A user-supplied
// --append-system-prompt is merged after ours; with --append-system-prompt-file
// or a subcommand (remote-control, mcp, …) the arguments are left alone.
func withSystemPrompt(args []string, prompt string) ([]string, bool) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args, false
	}
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			out = append(out, args[i:]...)
			i = len(args)
		case strings.HasPrefix(a, "--append-system-prompt-file"):
			return args, false
		case a == "--append-system-prompt" && i+1 < len(args):
			prompt += "\n\n" + args[i+1]
			i++
		case strings.HasPrefix(a, "--append-system-prompt="):
			prompt += "\n\n" + strings.TrimPrefix(a, "--append-system-prompt=")
		default:
			out = append(out, a)
		}
	}
	return append([]string{"--append-system-prompt", prompt}, out...), true
}
