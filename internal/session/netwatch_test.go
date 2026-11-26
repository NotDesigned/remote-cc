package session

import (
	"sync"
	"testing"
	"time"
)

// A change of the addresses is reported once; a failed listing is not a
// change, nor is the first one after it.
func TestWatchAddrs(t *testing.T) {
	var mu sync.Mutex
	seq := []struct {
		s  string
		ok bool
	}{{"a", true}, {"a", true}, {"", false}, {"a", true}, {"b", true}, {"b", true}}
	i := 0
	addrs := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		r := seq[min(i, len(seq)-1)]
		i++
		return r.s, r.ok
	}
	changes := make(chan struct{}, 10)
	stop := make(chan struct{})
	go watchAddrs(stop, time.Millisecond, addrs, func() { changes <- struct{}{} })
	time.Sleep(100 * time.Millisecond)
	close(stop)
	if n := len(changes); n != 1 {
		t.Fatalf("%d changes, want 1", n)
	}
	if s, ok := networkAddrs(); !ok || s == "x" {
		t.Fatalf("networkAddrs: %q %v", s, ok)
	}
}
