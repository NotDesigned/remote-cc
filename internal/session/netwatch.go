package session

import (
	"net"
	"slices"
	"strings"
	"time"
)

// watchNetwork calls changed when this computer's network addresses change
// (a Wi-Fi switch, a VPN going up or down, a cable): the connection to the
// server probably went with the old address, and the operating system may
// not say so for a long time. It checks every `every` until stop is closed.
func watchNetwork(stop <-chan struct{}, every time.Duration, changed func()) {
	watchAddrs(stop, every, networkAddrs, changed)
}

func watchAddrs(stop <-chan struct{}, every time.Duration, addrs func() (string, bool), changed func()) {
	if every < 0 {
		return
	}
	last, ok := addrs()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		now, nok := addrs()
		if !nok {
			continue
		}
		if ok && now != last {
			changed()
		}
		last, ok = now, true
	}
}

// networkAddrs lists the addresses of the interfaces that are up, loopback
// excluded, as one comparable string.
func networkAddrs() (string, bool) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	var addrs []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		as, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			addrs = append(addrs, ifc.Name+"="+a.String())
		}
	}
	slices.Sort(addrs)
	return strings.Join(addrs, " "), true
}
