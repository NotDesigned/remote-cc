package cli

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	var acct string
	var np bool
	var keys []string
	tg, pos, after, err := parse([]string{"-vvv", "-p", "22", "--account", "w", "--key", "a", "-6", "host", "--key", "b", "dir", "--no-prompt", "--", "--resume", "-x"},
		flags{str: map[string]*string{"--account": &acct}, bools: map[string]*bool{"--no-prompt": &np}, list: map[string]*[]string{"--key": &keys}})
	if err != nil {
		t.Fatal(err)
	}
	if !tg.Verbose || !reflect.DeepEqual(tg.Args, []string{"-o", "LogLevel=DEBUG2", "-p", "22", "-6"}) {
		t.Fatalf("target %+v", tg)
	}
	if acct != "w" || !np || !reflect.DeepEqual(keys, []string{"a", "b"}) {
		t.Fatalf("flags %q %v %v", acct, np, keys)
	}
	if !reflect.DeepEqual(pos, []string{"host", "dir"}) || !reflect.DeepEqual(after, []string{"--resume", "-x"}) {
		t.Fatalf("pos %v after %v", pos, after)
	}
	if _, _, _, err := parse([]string{"--bogus"}, flags{}); err == nil {
		t.Fatal("unknown option accepted")
	}
	if _, _, _, err := parse([]string{"-p"}, flags{}); err == nil {
		t.Fatal("missing value accepted")
	}
	if tg, _, _, _ := parse([]string{"-v"}, flags{}); !tg.Verbose || len(tg.Args) != 0 {
		t.Fatalf("-v: %+v", tg)
	}
}
