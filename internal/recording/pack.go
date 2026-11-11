package recording

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PackOptions scope what Pack writes. The zero value packs everything.
type PackOptions struct {
	Models []string  // keep only these models, by name
	Units  []string  // keep only these units or applications: their unit probe and workload logs, and the lines of the other sources that name them
	Since  time.Time // keep only lines at or after this time
	Until  time.Time // keep only lines at or before this time
}

func (o PackOptions) scoped() bool {
	return len(o.Models) > 0 || len(o.Units) > 0 || !o.Since.IsZero() || !o.Until.IsZero()
}

// PackResult says what Pack wrote.
type PackResult struct {
	Files    int   // files in the archive, the manifest included
	RawFiles int   // files of raw/ among them, status snapshots not counted
	Bytes    int64 // before compression
}

// Pack writes the recording at dir to w as a gzipped tar archive holding what
// can't be rebuilt: manifest.json, raw/ and recorder.log. The index is left
// out, because it is rebuilt from raw/ the first time the recording is read.
// Everything is under one directory named after the recording, so extracting
// the archive gives a recording directory.
//
// With a scope, files of other models are left out, as are those of other
// units, and lines outside the time window; status snapshots of a kept model
// stay whole. A line that continues the one before it, as in a multi-line log
// message, is kept or dropped with it. The archive's manifest says it is
// scoped.
func Pack(dir string, w io.Writer, opts PackOptions) (PackResult, error) {
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
	sc := newScoper(dir, opts)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	write := func(rel string, info fs.FileInfo, size int64, body io.Reader) error {
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(top, rel))
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.Size = size
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		n, err := io.Copy(tw, io.LimitReader(body, size))
		res.Files++
		res.Bytes += n
		return err
	}

	// The manifest, with the scope noted when there is one.
	mpath := layout.Manifest()
	minfo, err := os.Stat(mpath)
	if err != nil {
		return res, err
	}
	mdata, err := os.ReadFile(mpath)
	if err != nil {
		return res, err
	}
	if opts.scoped() {
		var m Manifest
		if err := json.Unmarshal(mdata, &m); err != nil {
			return res, fmt.Errorf("reading manifest: %w", err)
		}
		m.Exported = &ExportScope{Models: opts.Models, Units: opts.Units, Since: opts.Since.UTC(), Until: opts.Until.UTC()}
		if mdata, err = json.MarshalIndent(m, "", "  "); err != nil {
			return res, err
		}
	}
	if err := write(ManifestFilename, minfo, int64(len(mdata)), bytes.NewReader(mdata)); err != nil {
		return res, err
	}

	for _, name := range []string{"raw", "recorder.log"} {
		root := filepath.Join(dir, name)
		if _, err := os.Stat(root); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return res, err
		}
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			srel := filepath.ToSlash(rel)
			keep, filter, transform := sc.file(srel)
			if !keep {
				return nil
			}
			// Files of the status snapshots say nothing about whether a scope
			// matched anything.
			counted := !strings.HasPrefix(srel, "raw/status/") && strings.HasPrefix(srel, "raw/")
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if filter == nil && transform == nil {
				if counted {
					res.RawFiles++
				}
				return write(rel, info, info.Size(), f)
			}
			var buf bytes.Buffer
			if transform != nil {
				data, err := io.ReadAll(f)
				if err != nil {
					return err
				}
				out, err := transform(data)
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				buf.Write(out)
			} else if err := filterLines(f, &buf, filter); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if buf.Len() == 0 {
				return nil
			}
			if counted {
				res.RawFiles++
			}
			return write(rel, info, int64(buf.Len()), &buf)
		}); err != nil {
			return res, fmt.Errorf("packing %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return res, err
	}
	return res, gz.Close()
}

// lineFilter decides, for a line of a file, whether to keep it. ok is false
// for a line it can't read, which then follows the line before.
type lineFilter func(line []byte) (keep, ok bool)

// filterLines copies the lines of r that keep says to keep to w.
func filterLines(r io.Reader, w io.Writer, keep lineFilter) error {
	br := bufio.NewReaderSize(r, 1<<20)
	last := true
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			k, ok := keep(bytes.TrimRight(line, "\r\n"))
			if !ok {
				k = last
			}
			last = k
			if k {
				if _, werr := w.Write(line); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scoper applies PackOptions to the files of a recording.
type scoper struct {
	dir    string
	o      PackOptions
	models map[string]bool // sanitized model names to keep
	units  []string
	hosts  map[string][]string // unit probe directory -> units its probe covers
}

func newScoper(dir string, o PackOptions) *scoper {
	s := &scoper{dir: dir, o: o, units: o.Units, hosts: map[string][]string{}}
	if len(o.Models) > 0 {
		s.models = map[string]bool{}
		for _, m := range o.Models {
			s.models[sanitizeSegment(m)] = true
		}
	}
	return s
}

func (s *scoper) unitMatches(unit string) bool {
	for _, u := range s.units {
		if u == unit || unit != "" && strings.HasPrefix(unit, u+"/") {
			return true
		}
	}
	return false
}

// hostUnits are the units a unit probe's directory covers: what the probe said
// in its hello, or the unit a Kubernetes pod name gives.
func (s *scoper) hostUnits(model, host string) []string {
	key := model + "/" + host
	if u, ok := s.hosts[key]; ok {
		return u
	}
	var units []string
	dir := filepath.Join(s.dir, "raw", "unit", model, host)
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			if units = helloUnits(filepath.Join(dir, e.Name())); len(units) > 0 {
				break
			}
		}
	}
	if len(units) == 0 {
		units = []string{unitFromPod(host)}
	}
	s.hosts[key] = units
	return units
}

// helloUnits reads the units a unit probe covers from its hello record, among
// the first lines of its file.
func helloUnits(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for i := 0; i < 20 && sc.Scan(); i++ {
		var rec struct {
			Kind  string `json:"kind"`
			Hello struct {
				Units []string `json:"units"`
			} `json:"hello"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Kind == "hello" {
			return rec.Hello.Units
		}
	}
	return nil
}

func (s *scoper) anyHostUnit(model, host string) bool {
	for _, u := range s.hostUnits(model, host) {
		if s.unitMatches(u) {
			return true
		}
	}
	return false
}

func (s *scoper) inWindow(ts time.Time) bool {
	return (s.o.Since.IsZero() || !ts.Before(s.o.Since)) && (s.o.Until.IsZero() || !ts.After(s.o.Until))
}

// file says whether to keep the file at rel (under the recording, with
// slashes), and a filter for its lines, or a transform of its whole content,
// when only some of it is kept.
func (s *scoper) file(rel string) (keep bool, filter lineFilter, transform func([]byte) ([]byte, error)) {
	if !s.o.scoped() {
		return true, nil, nil
	}
	p := strings.Split(rel, "/")
	if p[0] != "raw" || len(p) < 4 {
		return true, nil, nil // recorder.log
	}
	kind, model := p[1], p[2]
	if s.models != nil && !s.models[model] {
		return false, nil, nil
	}
	timeOnly := !s.o.Since.IsZero() || !s.o.Until.IsZero()
	switch kind {
	case "status":
		if p[3] == "status-log.json" && (len(s.units) > 0 || timeOnly) {
			return true, nil, s.filterStatusLog
		}
		return true, nil, nil
	case "unit":
		if len(s.units) > 0 && !s.anyHostUnit(model, p[3]) {
			return false, nil, nil
		}
		if !timeOnly {
			return true, nil, nil
		}
		return true, func(line []byte) (bool, bool) {
			var r struct {
				Kind string    `json:"kind"`
				Ts   time.Time `json:"ts"`
			}
			if json.Unmarshal(line, &r) != nil || r.Ts.IsZero() {
				return false, false
			}
			// What the probe is and what it covers stays, whatever the time.
			return r.Kind == "hello" || r.Kind == "loader" || s.inWindow(r.Ts), true
		}, nil
	case "rpc":
		if !timeOnly && len(s.units) == 0 {
			return true, nil, nil
		}
		return true, func(line []byte) (bool, bool) {
			var r struct {
				Ts   time.Time `json:"ts"`
				Unit string    `json:"unit"`
			}
			if json.Unmarshal(line, &r) != nil || r.Ts.IsZero() {
				return false, false
			}
			return (len(s.units) == 0 || s.unitMatches(r.Unit)) && s.inWindow(r.Ts), true
		}, nil
	case "k8s":
		if len(s.units) > 0 && !s.unitMatches(unitFromPod(p[3])) {
			return false, nil, nil
		}
		return s.logFilter(rel, false, timeOnly)
	case "machine":
		if len(s.units) > 0 && !s.anyHostUnit(model, p[3]) {
			return false, nil, nil
		}
		return s.logFilter(rel, false, timeOnly)
	case "juju":
		return s.logFilter(rel, len(s.units) > 0, timeOnly)
	}
	return true, nil, nil
}

// logFilter keeps the lines of a log file in the time window and, when
// byUnit, those of the scope's units.
func (s *scoper) logFilter(rel string, byUnit, timeOnly bool) (bool, lineFilter, func([]byte) ([]byte, error)) {
	if !byUnit && !timeOnly {
		return true, nil, nil
	}
	return true, func(line []byte) (bool, bool) {
		rec, ok := ParseLogLine(rel, string(line))
		if !ok {
			return false, false
		}
		if byUnit && !s.unitMatches(rec.Unit) {
			return false, true
		}
		return s.inWindow(rec.Ts), true
	}, nil
}

// filterStatusLog keeps the entries of the units in the scope that are in the
// time window, in raw/status/<model>/status-log.json: an object that maps each
// unit to the entries of its status log.
func (s *scoper) filterStatusLog(data []byte) ([]byte, error) {
	var logs map[string][]json.RawMessage
	if err := json.Unmarshal(data, &logs); err != nil {
		return nil, err
	}
	out := map[string][]json.RawMessage{}
	for unit, entries := range logs {
		if len(s.units) > 0 && !s.unitMatches(unit) {
			continue
		}
		for _, e := range entries {
			var h struct {
				Since time.Time `json:"since"`
			}
			if json.Unmarshal(e, &h) == nil && !h.Since.IsZero() && !s.inWindow(h.Since) {
				continue
			}
			out[unit] = append(out[unit], e)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
}

// Models lists the models a recording has files of under raw/, as the names
// of their directories.
func Models(dir string) []string {
	seen := map[string]bool{}
	var out []string
	ents, _ := os.ReadDir(filepath.Join(dir, "raw"))
	for _, k := range ents {
		subs, _ := os.ReadDir(filepath.Join(dir, "raw", k.Name()))
		for _, m := range subs {
			if m.IsDir() && !seen[m.Name()] {
				seen[m.Name()] = true
				out = append(out, m.Name())
			}
		}
	}
	return out
}

// HasModel reports whether the recording has files of the model.
func HasModel(dir, model string) bool {
	for _, m := range Models(dir) {
		if m == sanitizeSegment(model) {
			return true
		}
	}
	return false
}
