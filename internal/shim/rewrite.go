package shim

import (
	"regexp"
	"strings"
)

// Claude Code (verified on 2.1.x) invokes CLAUDE_CODE_SHELL_PREFIX as
//
//	bash -c -l "'<prefix>' '<cmd>'"
//
// where <cmd> looks like:
//
//	source /root/.claude/shell-snapshots/snapshot-bash-….sh 2>/dev/null || true
//	  && { …setup… } && eval '<user command>' < /dev/null
//	  && pwd -P >| /tmp/claude-xxxx-cwd
//
// The snapshot captures B's shell environment and must not run on A. The
// trailing `pwd -P >| FILE` is how Claude Code tracks `cd`; FILE must end up
// on B, so we redirect it to a temp file on A and copy the result back.

var (
	snapshotRe = regexp.MustCompile(`^source ('[^']*'|\S+) 2>/dev/null \|\| true && `)
	cwdRe      = regexp.MustCompile(` && pwd -P >\| ('[^']*'|\S+)$`)
)

type Rewritten struct {
	Cmd string
	// CwdFile is the path on B that Claude Code expects the final cwd in;
	// empty if the command had no cwd-tracking suffix.
	CwdFile string
}

func Rewrite(cmd string) Rewritten {
	var r Rewritten
	if m := snapshotRe.FindStringSubmatch(cmd); m != nil && strings.Contains(m[1], "snapshot") {
		cmd = cmd[len(m[0]):]
	}
	if m := cwdRe.FindStringSubmatchIndex(cmd); m != nil {
		r.CwdFile = unquote(cmd[m[2]:m[3]])
		cmd = cmd[:m[0]] + ` && pwd -P >| "$RCC_CWD_FILE"`
	}
	r.Cmd = cmd
	return r
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}
