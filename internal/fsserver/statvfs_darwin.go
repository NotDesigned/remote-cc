package fsserver

import (
	"github.com/pkg/sftp"
	"golang.org/x/sys/unix"
)

func statVFS(p string) (*sftp.StatVFS, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(p, &s); err != nil {
		return nil, err
	}
	return &sftp.StatVFS{
		Bsize: uint64(s.Bsize), Frsize: uint64(s.Bsize),
		Blocks: s.Blocks, Bfree: s.Bfree, Bavail: s.Bavail,
		Files: s.Files, Ffree: s.Ffree, Favail: s.Ffree,
		Namemax: 255,
	}, nil
}
