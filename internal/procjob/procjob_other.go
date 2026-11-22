//go:build !windows

package procjob

func killOnExit() error { return nil }
