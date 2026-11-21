//go:build !linux && !darwin

package fsserver

import "github.com/pkg/sftp"

func statVFS(string) (*sftp.StatVFS, error) { return nil, sftp.ErrSSHFxOpUnsupported }
