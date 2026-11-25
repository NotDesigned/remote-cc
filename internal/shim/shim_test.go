package shim

import "testing"

func TestForwardEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/root", "TERM=xterm-256color",
		"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "__CLAUDE_X=1", "__ref=v",
		"CLAUDE_CODE_SHELL_PREFIX=/x/rcc-shim", "RCC_TOKEN=t", "RCC_EGRESS_GRANT=g",
		// The server account's credentials stay on the server.
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN=r",
		"ANTHROPIC_API_KEY=k", "ANTHROPIC_AUTH_TOKEN=a",
	}
	got := forwardEnv(env, "echo $__ref $ANTHROPIC_API_KEY")
	for _, k := range []string{"TERM", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "__CLAUDE_X", "__ref"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s not forwarded", k)
		}
	}
	for _, k := range []string{"PATH", "HOME", "CLAUDE_CODE_SHELL_PREFIX", "RCC_TOKEN", "RCC_EGRESS_GRANT",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s forwarded", k)
		}
	}
}
