package index

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/wire"
)

// Indexer incrementally brings an index.db up to date with a recording's raw/
// tree. It remembers how far it has consumed each raw file, so repeated Sync
// calls only process newly-appended bytes — this is what lets `record` keep the
// index fresh while capturing, and `view --follow` tail a live recording,
// instead of re-indexing the whole recording each time (VISION M5).
//
// It carries the pairing and per-unit hook state across Syncs so a request in
// one file/tick pairs with its response in a later one, and so a hook that
// begins in one tick labels the RPCs that follow it in the next.
type Indexer struct {
	db         *DB
	pairer     *recording.Pairer
	offsets    map[string]int64  // raw file (relative path) -> bytes consumed
	lineNo     map[string]int    // raw file -> lines consumed (for log RawLine)
	hookByUnit map[string]string // unit -> hook currently running
}

// NewIndexer returns an indexer writing to db.
func NewIndexer(db *DB) *Indexer {
	return &Indexer{
		db:         db,
		pairer:     recording.NewPairer(),
		offsets:    map[string]int64{},
		lineNo:     map[string]int{},
		hookByUnit: map[string]string{},
	}
}

// Sync consumes any raw bytes appended since the last call and writes the
// derived spans, snapshots and log records. It only reads complete (newline
// terminated) lines, so a half-written trailing line is left for the next Sync.
func (ix *Indexer) Sync(root string) error {
	l := recording.NewLayout(root)
	if err := ix.syncCalls(root, filepath.Join(l.RawDir(), "rpc")); err != nil {
		return err
	}
	return ix.syncLogs(root, filepath.Join(l.RawDir(), "juju"))
}

// FlushPending writes the still-open requests (no response captured) as
// zero-duration spans. Call once at end of recording.
func (ix *Indexer) FlushPending() error {
	for _, sp := range ix.pairer.Pending() {
		if err := ix.processSpan(sp); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Indexer) syncCalls(root, rpcRoot string) error {
	// Collect the spans completed this Sync, then process them in request-start
	// order so per-unit hook state (set from SetState) is applied before the
	// RPCs that follow within the hook. Cross-Sync ordering is handled by the
	// authoritative full rebuild the recorder runs at stop.
	var completed []recording.SpanRow
	err := ix.eachModelFile(root, rpcRoot, "calls-", ".jsonl", func(rel string, lines []lineAt) error {
		for _, ln := range lines {
			cm, err := wire.UnmarshalLine(ln.data)
			if err != nil {
				continue
			}
			if sp, ok := ix.pairer.Feed(cm, rel, ln.offset); ok {
				completed = append(completed, sp)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.SliceStable(completed, func(i, j int) bool { return completed[i].Start.Before(completed[j].Start) })
	for _, sp := range completed {
		if err := ix.processSpan(sp); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Indexer) syncLogs(root, jujuRoot string) error {
	return ix.eachModelFile(root, jujuRoot, "debug-log-", ".log", func(rel string, lines []lineAt) error {
		model := modelOfRel(rel)
		for _, ln := range lines {
			rec, ok := recording.ParseDebugLogLine(string(ln.data), model)
			if !ok {
				continue
			}
			ix.lineNo[rel]++
			rec.RawFile = rel
			rec.RawLine = ix.lineNo[rel]
			if err := ix.db.InsertLog(rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// processSpan enriches a completed span with hook + relation context and writes
// it and any snapshots it produced.
func (ix *Indexer) processSpan(sp recording.SpanRow) error {
	if sp.Attrs["method"] == "SetState" {
		if hook, op := parseUniterState(sp.Attrs["params"]); op != "" {
			if op == "run-hook" && hook != "" {
				ix.hookByUnit[sp.Unit] = hook
			} else {
				delete(ix.hookByUnit, sp.Unit)
			}
		}
	}
	if sp.Hook == "" {
		sp.Hook = ix.hookByUnit[sp.Unit]
	}
	if sp.Relation == "" {
		sp.Relation = RelationForSpan(sp)
	}
	if err := ix.db.InsertSpan(sp); err != nil {
		return err
	}
	for _, s := range SnapshotsForSpan(sp) {
		if err := ix.db.InsertSnapshot(s.Model, s.Ts, string(s.Kind), s.Scope, string(s.Body), s.ProducingSpanID); err != nil {
			return err
		}
	}
	return nil
}

type lineAt struct {
	data   []byte
	offset int64
}

// eachModelFile walks raw/<kind>/<model>/<prefix>*<suffix> in chronological
// order and calls fn with the newly-appended complete lines of each file,
// advancing the per-file offset.
func (ix *Indexer) eachModelFile(root, kindRoot, prefix, suffix string, fn func(rel string, lines []lineAt) error) error {
	modelDirs, err := os.ReadDir(kindRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, md := range modelDirs {
		if !md.IsDir() {
			continue
		}
		dir := filepath.Join(kindRoot, md.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(files))
		for _, f := range files {
			if !f.IsDir() && strings.HasPrefix(f.Name(), prefix) && strings.HasSuffix(f.Name(), suffix) {
				names = append(names, f.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			rel, _ := filepath.Rel(root, path)
			lines, newOff, err := readNewLines(path, ix.offsets[rel])
			if err != nil {
				continue
			}
			if len(lines) == 0 {
				continue
			}
			if err := fn(rel, lines); err != nil {
				return err
			}
			ix.offsets[rel] = newOff
		}
	}
	return nil
}

// readNewLines returns the complete lines in path starting at byte offset from,
// each with its start offset, plus the offset just past the last complete line.
// A trailing partial line (no terminating newline yet) is not consumed.
func readNewLines(path string, from int64) ([]lineAt, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, from, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, from, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, from, err
	}
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil, from, nil // no complete line yet
	}
	complete := buf[:end+1]
	var out []lineAt
	pos := from
	for _, line := range bytes.SplitAfter(complete, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		trimmed := line
		if trimmed[len(trimmed)-1] == '\n' {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if len(trimmed) > 0 {
			out = append(out, lineAt{data: append([]byte(nil), trimmed...), offset: pos})
		}
		pos += int64(len(line))
	}
	return out, from + int64(len(complete)), nil
}

// modelOfRel extracts the model segment from a "raw/<kind>/<model>/<file>"
// relative path.
func modelOfRel(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}
