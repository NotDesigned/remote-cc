// Package fsserver is the A-side half of the file channel: an in-process SFTP
// server that `sshfs -o slave` on B talks to over an ssh session's stdio.
// A therefore needs neither sshd nor sftp-server.
//
// SFTP paths are in B's form (see pathmap). Only the project subtree is
// served; that is a guard against mistakes, not a security boundary — the
// command channel can already run anything on A.
package fsserver

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"remote-cc/internal/pathmap"
)

// Serve speaks SFTP on rw until EOF. root is the served directory in B's form.
func Serve(rw io.ReadWriteCloser, root string) error {
	h := &handler{root: path.Clean(root), goos: runtime.GOOS}
	srv := sftp.NewRequestServer(rw, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h},
		sftp.WithStartDirectory(h.root))
	err := srv.Serve()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type handler struct {
	root string
	goos string
}

var errOutside = &fs.PathError{Op: "access", Err: fs.ErrPermission}

// local maps an SFTP path to a native path, refusing anything outside root.
func (h *handler) local(p string) (string, error) {
	p = path.Clean("/" + p)
	if p != h.root && !strings.HasPrefix(p, strings.TrimSuffix(h.root, "/")+"/") {
		e := *errOutside
		e.Path = p
		return "", &e
	}
	return pathmap.ToLocal(h.goos, p), nil
}

// ---- FileReader / FileWriter / OpenFileWriter

func (h *handler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	p, err := h.local(r.Filepath)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (h *handler) Filewrite(r *sftp.Request) (io.WriterAt, error) { return h.open(r) }

func (h *handler) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) { return h.open(r) }

func (h *handler) open(r *sftp.Request) (*os.File, error) {
	p, err := h.local(r.Filepath)
	if err != nil {
		return nil, err
	}
	pf := r.Pflags()
	var flag int
	switch {
	case pf.Read && pf.Write:
		flag = os.O_RDWR
	case pf.Write || pf.Append:
		flag = os.O_WRONLY
	default:
		flag = os.O_RDONLY
	}
	// O_APPEND is dropped on purpose: sshfs always sends explicit offsets,
	// and os.File.WriteAt refuses files opened with O_APPEND.
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	return os.OpenFile(p, flag, perm(r, 0o644))
}

// ---- FileCmder

func (h *handler) Filecmd(r *sftp.Request) error {
	p, err := h.local(r.Filepath)
	if r.Method != "Symlink" && err != nil {
		return err
	}
	switch r.Method {
	case "Setstat":
		return h.setstat(r, p)
	case "Rename":
		t, err := h.local(r.Target)
		if err != nil {
			return err
		}
		// Plain SFTP rename must not overwrite (posix-rename does).
		if _, err := os.Lstat(t); err == nil {
			return &fs.PathError{Op: "rename", Path: r.Target, Err: fs.ErrExist}
		}
		return os.Rename(p, t)
	case "Rmdir", "Remove":
		return os.Remove(p)
	case "Mkdir":
		return os.Mkdir(p, perm(r, 0o755))
	case "Link":
		t, err := h.local(r.Target)
		if err != nil {
			return err
		}
		return os.Link(p, t)
	case "Symlink":
		// Filepath is the link's target, stored verbatim; Target is the new link.
		link, err := h.local(r.Target)
		if err != nil {
			return err
		}
		target := r.Filepath
		if h.goos == "windows" && strings.HasPrefix(target, "/") {
			target = pathmap.ToLocal(h.goos, target)
		}
		return os.Symlink(target, link)
	}
	return sftp.ErrSSHFxOpUnsupported
}

func (h *handler) PosixRename(r *sftp.Request) error {
	p, err := h.local(r.Filepath)
	if err != nil {
		return err
	}
	t, err := h.local(r.Target)
	if err != nil {
		return err
	}
	return os.Rename(p, t)
}

func (h *handler) StatVFS(r *sftp.Request) (*sftp.StatVFS, error) {
	p, err := h.local(r.Filepath)
	if err != nil {
		return nil, err
	}
	return statVFS(p)
}

func (h *handler) setstat(r *sftp.Request, p string) error {
	f := r.AttrFlags()
	a := r.Attributes()
	if a == nil {
		return nil
	}
	if f.Size {
		if err := os.Truncate(p, int64(a.Size)); err != nil {
			return err
		}
	}
	if f.Permissions {
		if err := os.Chmod(p, a.FileMode().Perm()); err != nil {
			return err
		}
	}
	if f.Acmodtime {
		if err := os.Chtimes(p, time.Unix(int64(a.Atime), 0), time.Unix(int64(a.Mtime), 0)); err != nil {
			return err
		}
	}
	// UidGid is ignored: B displays every file as its own user (sshfs -o uid=,gid=).
	return nil
}

// perm returns the permission bits sent with the request, or def.
func perm(r *sftp.Request, def fs.FileMode) fs.FileMode {
	if a := r.Attributes(); a != nil && r.AttrFlags().Permissions {
		return a.FileMode().Perm()
	}
	return def
}

// ---- FileLister

func (h *handler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p, err := h.local(r.Filepath)
	if err != nil {
		return nil, err
	}
	switch r.Method {
	case "List":
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		infos := make([]fs.FileInfo, 0, len(ents))
		for _, e := range ents {
			if fi, err := e.Info(); err == nil { // vanished entries are skipped
				infos = append(infos, fi)
			}
		}
		return lister(infos), nil
	case "Stat":
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		return lister{fi}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (h *handler) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	p, err := h.local(r.Filepath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	return lister{fi}, nil
}

func (h *handler) Readlink(p string) (string, error) {
	lp, err := h.local(p)
	if err != nil {
		return "", err
	}
	t, err := os.Readlink(lp)
	if err != nil {
		return "", err
	}
	if h.goos == "windows" && len(t) >= 2 && t[1] == ':' {
		t = pathmap.ToRemote(h.goos, t)
	}
	return t, nil
}

type lister []fs.FileInfo

func (l lister) ListAt(dst []fs.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[off:])
	if off+int64(n) >= int64(len(l)) {
		return n, io.EOF
	}
	return n, nil
}
