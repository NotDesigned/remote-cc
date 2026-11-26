package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSudo needs a password ("right"). Like the real one it tries a wrong
// password three times, and with -S reads each try from stdin.
const fakeSudo = `#!/bin/sh
echo "sudo $*" >> "$FAKE/sudo.log"
[ "$1" = -n ] && exit 1
mode=$1; shift; [ "$1" = -p ] && shift 2
for i in 1 2 3; do
  case "$mode" in
    -A) pw=$("$SUDO_ASKPASS") ;;
    -S) IFS= read -r pw || exit 1 ;;
  esac
  [ "$pw" = right ] && exec env -u RCC_SUDO_PW "$@"
done
exit 1
`

func runRoot(t *testing.T, stdin string) (string, string) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	d := t.TempDir()
	bin := filepath.Join(d, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o755)
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 1000\n"), 0o755)
	body := "IFS= read -r tok; echo \"script got $tok\"\n"
	cmd := exec.Command("sh", "-c", Join("lib/root.sh")+body)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+d, "HAVE_PASS=1",
		"SELF_B64="+b64(body), "TMPDIR="+d)
	cmd.Stdin = strings.NewReader(stdin)
	out, _ := cmd.Output()
	log, _ := os.ReadFile(filepath.Join(d, "sudo.log"))
	if left, _ := filepath.Glob(filepath.Join(d, "tmp.*")); len(left) != 0 {
		t.Errorf("askpass helper left behind: %v", left)
	}
	return string(out), string(log)
}

// A wrong sudo password must not make sudo read the token meant for the
// script as another try; the right one runs the script with the token.
func TestSudoPasswordNeverConsumesInput(t *testing.T) {
	out, log := runRoot(t, "wrong\nrcch_secret\n")
	if !strings.Contains(out, "rcc-wrong-sudo-password") || strings.Contains(out, "script got") {
		t.Fatalf("wrong password: %q", out)
	}
	if strings.Contains(log, "-S") {
		t.Fatalf("sudo read the password from stdin: %q", log)
	}
	out, _ = runRoot(t, "right\nrcch_secret\n")
	if !strings.Contains(out, "script got rcch_secret") {
		t.Fatalf("right password: %q", out)
	}
}
