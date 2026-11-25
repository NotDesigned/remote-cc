//go:build !windows

package procjob

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	once  sync.Once
	mu    sync.Mutex
	pipeW *os.File // write end; closed by the kernel when rcc dies
	err   error
)

// A process started as `<exe> __rcc_guard` is a guard whatever the
// executable is. killOnExit starts os.Executable(), which in tests is the
// test binary: without this it would run the tests again, which start
// another guard, and so on.
func init() {
	if len(os.Args) == 2 && os.Args[1] == GuardArg {
		RunGuard()
		os.Exit(0)
	}
}

func killOnExit() error {
	once.Do(func() {
		exe, e := os.Executable()
		if e != nil {
			err = e
			return
		}
		r, w, e := os.Pipe()
		if e != nil {
			err = e
			return
		}
		cmd := exec.Command(exe, GuardArg)
		cmd.Stdin = r
		// Own session: not hit by the terminal's SIGHUP/SIGINT, not part of
		// rcc's process group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if e := cmd.Start(); e != nil {
			r.Close()
			w.Close()
			err = e
			return
		}
		r.Close()
		// Reap the guard if it ever exits while we run; don't block.
		go cmd.Wait()
		pipeW = w
	})
	return err
}

func track(kind byte, id int) {
	mu.Lock()
	defer mu.Unlock()
	if pipeW == nil || id <= 0 {
		return
	}
	fmt.Fprintf(pipeW, "%c%d\n", kind, id)
}

func trackDir(dir string) {
	mu.Lock()
	defer mu.Unlock()
	if pipeW == nil || strings.ContainsAny(dir, "\n") {
		return
	}
	fmt.Fprintf(pipeW, "d%s\n", dir)
}

func safeTempDir(dir string) bool {
	return filepath.Dir(dir) == filepath.Clean(os.TempDir()) && strings.HasPrefix(filepath.Base(dir), "rcc-")
}

// RunGuard is the guard process main loop (Unix only).
func RunGuard() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE, syscall.SIGQUIT)

	var gmu sync.Mutex
	targets := map[int]bool{} // negative = process group, positive = process
	var dirs []string

	alive := func(t int) bool { return syscall.Kill(t, 0) != syscall.ESRCH }

	// Forget targets that are gone, so a recycled PID is never signalled.
	go func() {
		for range time.Tick(10 * time.Second) {
			gmu.Lock()
			for t := range targets {
				if !alive(t) {
					delete(targets, t)
				}
			}
			gmu.Unlock()
		}
	}()

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		l := sc.Text()
		if len(l) < 2 {
			continue
		}
		if l[0] == 'd' {
			if safeTempDir(l[1:]) {
				gmu.Lock()
				dirs = append(dirs, l[1:])
				gmu.Unlock()
			}
			continue
		}
		id, e := strconv.Atoi(l[1:])
		if e != nil || id <= 0 {
			continue
		}
		gmu.Lock()
		switch l[0] {
		case 'g':
			targets[-id] = true
		case 'p':
			targets[id] = true
		}
		gmu.Unlock()
	}

	// EOF: rcc is gone.
	gmu.Lock()
	defer gmu.Unlock()
	defer func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}()
	var live []int
	for t := range targets {
		if alive(t) {
			syscall.Kill(t, syscall.SIGTERM)
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		any := false
		for _, t := range live {
			if alive(t) {
				any = true
			}
		}
		if !any {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, t := range live {
		syscall.Kill(t, syscall.SIGKILL)
	}
}
