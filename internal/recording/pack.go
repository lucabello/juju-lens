package recording

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// PackResult says what Pack wrote.
type PackResult struct {
	Files int
	Bytes int64 // before compression
}

// Pack writes the recording at dir to w as a gzipped tar archive holding what
// can't be rebuilt: manifest.json, raw/ and recorder.log. The index is left
// out, because it is rebuilt from raw/ the first time the recording is read. Everything is under one directory named
// after the recording, so extracting the archive gives a recording directory.
func Pack(dir string, w io.Writer) (PackResult, error) {
	var res PackResult
	layout := NewLayout(dir)
	if ok, err := layout.Exists(); err != nil {
		return res, err
	} else if !ok {
		return res, fmt.Errorf("no manifest.json under %q; is this a juju-lens recording?", dir)
	}
	if pid, err := ReadPidFile(layout.PidFile()); err == nil && PidAlive(pid) {
		return res, fmt.Errorf("a recorder (PID %d) is still writing to %s; stop it with `juju-lens stop %s` first", pid, dir, dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return res, err
	}
	top := filepath.Base(abs)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(path string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(top, rel))
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if d.IsDir() {
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		hdr.Size = info.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		n, err := io.Copy(tw, io.LimitReader(f, info.Size()))
		res.Files++
		res.Bytes += n
		return err
	}
	for _, name := range []string{ManifestFilename, "raw", "recorder.log"} {
		root := filepath.Join(dir, name)
		if _, err := os.Stat(root); err != nil {
			if os.IsNotExist(err) && name != ManifestFilename {
				continue
			}
			return res, err
		}
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return add(path, d)
		}); err != nil {
			return res, fmt.Errorf("packing %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return res, err
	}
	return res, gz.Close()
}
