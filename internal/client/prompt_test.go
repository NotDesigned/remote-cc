package client

import (
	"reflect"
	"strings"
	"testing"
)

func TestWithSystemPrompt(t *testing.T) {
	got, ok := withSystemPrompt([]string{"-p", "hi"}, "P")
	if !ok || !reflect.DeepEqual(got, []string{"--append-system-prompt", "P", "-p", "hi"}) {
		t.Fatalf("%q", got)
	}
	got, _ = withSystemPrompt([]string{"--append-system-prompt", "U", "-c"}, "P")
	if !reflect.DeepEqual(got, []string{"--append-system-prompt", "P\n\nU", "-c"}) {
		t.Fatalf("merge: %q", got)
	}
	got, _ = withSystemPrompt([]string{"--append-system-prompt=U"}, "P")
	if !reflect.DeepEqual(got, []string{"--append-system-prompt", "P\n\nU"}) {
		t.Fatalf("merge=: %q", got)
	}
	got, _ = withSystemPrompt([]string{"-p", "--", "--append-system-prompt"}, "P")
	if !reflect.DeepEqual(got, []string{"--append-system-prompt", "P", "-p", "--", "--append-system-prompt"}) {
		t.Fatalf("after --: %q", got)
	}
	for _, args := range [][]string{{"remote-control"}, {"mcp", "list"}, {"--append-system-prompt-file", "f"}} {
		if got, ok := withSystemPrompt(args, "P"); ok || !reflect.DeepEqual(got, args) {
			t.Fatalf("should be untouched: %q", got)
		}
	}
	if got, _ := withSystemPrompt(nil, "P"); !reflect.DeepEqual(got, []string{"--append-system-prompt", "P"}) {
		t.Fatalf("empty: %q", got)
	}
}

func TestSystemPromptMentionsEssentials(t *testing.T) {
	p := systemPrompt("/home/me/proj", "/usr/bin/bash")
	for _, w := range []string{"/home/me/proj", "/usr/bin/bash", "Bash command runs on the user's machine", "outside it act on the remote host"} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q:\n%s", w, p)
		}
	}
}
