package client

import (
	"strings"
	"testing"

	"remote-cc/internal/scripts"
)

// client/session.sh and the client agree on the "being replaced" mark.
func TestUpdatingMark(t *testing.T) {
	if !strings.Contains(scripts.Get("client/session.sh"), `echo "`+updatingMark+`" >&2; exit 75`) {
		t.Fatal("client/session.sh does not print the mark the client looks for")
	}
	if !updating("Warning: something\n" + updatingMark + "\n") {
		t.Error("mark not recognised")
	}
	if updating("rcc: rcc-shim is missing on the server\n") {
		t.Error("other start failures are not updates")
	}
}
