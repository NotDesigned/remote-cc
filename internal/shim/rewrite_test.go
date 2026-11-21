package shim

import "testing"

// Captured verbatim from Claude Code 2.1.330.
const captured = `source /root/.claude/shell-snapshots/snapshot-bash-1795279000419-hc5fxc.sh 2>/dev/null || true && { \builtin shopt -u extglob || \builtin setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL NO_BRACE_CCL NO_MAGIC_EQUAL_SUBST NO_EQUALS; } >/dev/null 2>&1 || true && { \builtin unalias -- 'which'; \builtin unset -f -- 'which'; } >/dev/null 2>&1 || true && eval 'cd /tmp && echo hello-from-bash' < /dev/null && pwd -P >| /tmp/claude-b614-cwd`

func TestRewriteCaptured(t *testing.T) {
	r := Rewrite(captured)
	if r.CwdFile != "/tmp/claude-b614-cwd" {
		t.Fatalf("CwdFile = %q", r.CwdFile)
	}
	want := `{ \builtin shopt -u extglob || \builtin setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL NO_BRACE_CCL NO_MAGIC_EQUAL_SUBST NO_EQUALS; } >/dev/null 2>&1 || true && { \builtin unalias -- 'which'; \builtin unset -f -- 'which'; } >/dev/null 2>&1 || true && eval 'cd /tmp && echo hello-from-bash' < /dev/null && pwd -P >| "$RCC_CWD_FILE"`
	if r.Cmd != want {
		t.Fatalf("Cmd =\n%s\nwant\n%s", r.Cmd, want)
	}
}

func TestRewriteQuotedPaths(t *testing.T) {
	r := Rewrite(`source '/home/a b/.claude/shell-snapshots/s.sh' 2>/dev/null || true && eval ls && pwd -P >| '/tmp/x y-cwd'`)
	if r.CwdFile != "/tmp/x y-cwd" || r.Cmd != `eval ls && pwd -P >| "$RCC_CWD_FILE"` {
		t.Fatalf("%+v", r)
	}
}

func TestRewritePassthrough(t *testing.T) {
	r := Rewrite("echo hi")
	if r.Cmd != "echo hi" || r.CwdFile != "" {
		t.Fatalf("%+v", r)
	}
}
