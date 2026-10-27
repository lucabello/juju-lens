//go:build linux

package recording

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// invoker returns the uid and gid of the user who ran sudo, when this process
// is root because of it.
func invoker() (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	u, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	g, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil || u == 0 {
		return 0, 0, false
	}
	return u, g, true
}

// HandToInvoker gives the recording at root to the user who ran sudo, so that
// `view` and `index` run as that user can open and write index.db. Recording
// needs root for eBPF, but without this everything in the recording ends up
// root-owned and unusable to the person who made it. It does nothing unless
// this process is root through sudo. Run it again after files have been
// added: new ones are owned by root.
func HandToInvoker(root string) error {
	uid, gid, ok := invoker()
	if !ok {
		return nil
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// OwnerHint explains why a recording this process cannot write to cannot be
// opened, or returns "" if the cause is not ownership.
func OwnerHint(root string) string {
	if os.Geteuid() == 0 {
		return ""
	}
	fi, err := os.Stat(filepath.Join(root, "index.db"))
	if err != nil {
		fi, err = os.Stat(filepath.Join(root, "manifest.json"))
		if err != nil {
			return ""
		}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == os.Geteuid() {
		return ""
	}
	return fmt.Sprintf("%s is owned by uid %d, probably because it was recorded under sudo; "+
		"run `sudo chown -R $(id -u):$(id -g) %s` to take it over", root, st.Uid, root)
}
