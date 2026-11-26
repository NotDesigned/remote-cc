package agent

import (
	"strings"
	"testing"
)

// The session log (last.log) names each command; secrets in it stay out.
func TestSummarizeHidesSecrets(t *testing.T) {
	for _, c := range []string{
		"curl -H 'Authorization: Bearer abc123secret' x",
		"export GITHUB_TOKEN=abc123secret",
		"mysql --password=abc123secret",
		"echo sk-ant-abc123secret",
		"rcc rcb login --token rcck_abc123secret",
	} {
		if s := summarize(c); strings.Contains(s, "abc123secret") {
			t.Errorf("%q -> %q", c, s)
		}
	}
	if s := summarize("git status"); s != "git status" {
		t.Errorf("plain command changed: %q", s)
	}
}
