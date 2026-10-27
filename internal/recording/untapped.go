package recording

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/unit"
)

// What the tap recorded of a hook, in a root span's Attrs["tap"], with the
// reason in Attrs["tap_reason"] when it is known. A root with neither was
// recorded in full.
const (
	// TapMissing: the hook ran on a unit the unit probes covered, but the
	// tap recorded nothing of it. The root stands in for the process.
	TapMissing = "missing"
	// TapTruncated: the tap's records of the process stop before its
	// proc_end.
	TapTruncated = "truncated"
)

// Who saw a hook the tap missed, in Attrs["seen_by"], comma-separated.
const (
	SeenByStatusLog = "status-log" // `juju show-status-log --type juju-unit`
	SeenByRPC       = "rpc"        // the uniter's run-hook marker
	SeenByProc      = "proc"       // the unit probe's /proc poll: a root with exact bounds and resources
)

// Reasons the tap missed a hook, in Attrs["tap_reason"].
const (
	ReasonJujuExec      = "juju exec runs commands, not the charm"
	ReasonBeforeProbe   = "the unit probe hadn't started yet"
	ReasonAfterProbe    = "the unit probe had stopped"
	ReasonNoSession     = "no unit probe session was running"
	ReasonPodStart      = "the tap wasn't installed yet: the pod had just started"
	ReasonNotInstalled  = "the tap wasn't installed yet"
	ReasonVenvReplaced  = "the tap wasn't back yet in the venv a refresh replaced"
	ReasonRemoved       = "the tap had been removed"
	reasonSkippedPrefix = "the tap can't be installed: "
	reasonUnavailPrefix = "the tap couldn't be set up on the host: "
)

// truncation reasons, in Attrs["tap_reason"] of a TapTruncated root.
const (
	ReasonKilled     = "it exited without proc_end: killed (SIGKILL, out of memory) or ended without running its exit handlers"
	ReasonSessionCut = "its unit probe session ended before it did, as when the pod is deleted"
)

// HookSighting is one hook or action run that something other than the tap
// saw: the unit agent's status log, or an RPC run-hook bracket.
type HookSighting struct {
	Model     string
	Unit      string
	Exec      string // ExecHook or ExecAction
	Name      string // charm-visible hook name, or the action name; "" when only Kind is known
	Kind      string // bare hook kind ("relation-changed"), or the action name
	RemoteApp string // relation hooks seen by RPC: the remote application
	Start     time.Time
	End       time.Time
	Failed    bool     // the status log says the hook failed
	SeenBy    []string // SeenByStatusLog, SeenByRPC
	RawFile   string   // relative to the recording
}

// statusLogEntry is one entry of `juju show-status-log --format=json`.
type statusLogEntry struct {
	Status  string    `json:"status"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// LoadStatusLog reads raw/status/<model>/status-log.json of every model and
// returns the hooks and actions the unit agents' status logs list from
// since on, ordered by start. A hook ends at the unit's next entry.
// update-status never appears: Juju doesn't log it.
func LoadStatusLog(root string, since time.Time) ([]HookSighting, error) {
	paths, _ := filepath.Glob(filepath.Join(NewLayout(root).RawDir(), "status", "*", "status-log.json"))
	sort.Strings(paths)
	var out []HookSighting
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var byUnit map[string][]statusLogEntry
		if err := json.Unmarshal(data, &byUnit); err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		model := filepath.Base(filepath.Dir(p))
		for u, entries := range byUnit {
			out = append(out, statusLogSightings(model, u, entries, since, filepath.ToSlash(rel))...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func statusLogSightings(model, unitName string, entries []statusLogEntry, since time.Time, rel string) []HookSighting {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Since.Before(entries[j].Since) })
	var out []HookSighting
	for i, e := range entries {
		if e.Status != "executing" || e.Since.Before(since) {
			continue
		}
		s := HookSighting{
			Model: model, Unit: unitName, Start: e.Since, End: e.Since,
			SeenBy: []string{SeenByStatusLog}, RawFile: rel,
		}
		switch {
		case strings.HasPrefix(e.Message, "running action "):
			s.Exec, s.Name = ExecAction, strings.TrimPrefix(e.Message, "running action ")
			s.Kind = s.Name
		case strings.HasPrefix(e.Message, "running ") && strings.HasSuffix(e.Message, " hook"):
			s.Exec, s.Name = ExecHook, strings.TrimSuffix(strings.TrimPrefix(e.Message, "running "), " hook")
			s.Kind = HookKind(s.Name)
		default:
			continue
		}
		if i+1 < len(entries) {
			next := entries[i+1]
			s.End = next.Since
			s.Failed = next.Status == "error" && strings.HasPrefix(next.Message, "hook failed")
		}
		out = append(out, s)
	}
	return out
}

// hookKindSuffixes are the hook kinds Juju prefixes with an endpoint,
// storage or container name.
var hookKindSuffixes = []string{
	"relation-created", "relation-joined", "relation-changed", "relation-departed", "relation-broken",
	"storage-attached", "storage-detaching",
	"pebble-ready", "pebble-custom-notice", "pebble-check-failed", "pebble-check-recovered",
}

// HookKind is the bare kind of a charm-visible hook name, as the uniter's
// run-hook marker carries it ("db-relation-changed" -> "relation-changed").
func HookKind(name string) string {
	for _, k := range hookKindSuffixes {
		if strings.HasSuffix(name, "-"+k) {
			return k
		}
	}
	return name
}

// UntappedRoot is the root span that stands in for a hook the tap recorded
// nothing of: an ops root with tap=missing, the reason, and who saw it.
func UntappedRoot(s HookSighting, reason string) SpanRow {
	h := fnv.New128a()
	h.Write([]byte("untapped\x00" + s.Model + "\x00" + s.Unit + "\x00" + s.Exec + "\x00" + s.Kind + "\x00" + s.Start.UTC().Format(time.RFC3339Nano)))
	sum := h.Sum(nil)
	name := s.Name
	if name == "" {
		name = s.Kind
	}
	hook := name
	if s.Exec == ExecAction {
		hook = name + " action"
	}
	end := s.End
	if end.Before(s.Start) {
		end = s.Start
	}
	attrs := map[string]string{
		"kind": OpsRoot, "exec": s.Exec, "tap": TapMissing,
		"seen_by": strings.Join(s.SeenBy, ","),
	}
	if s.Exec == ExecHook {
		attrs["hook_kind"] = s.Kind
	}
	if reason != "" {
		attrs["tap_reason"] = reason
	}
	if s.RemoteApp != "" {
		env, _ := json.Marshal(map[string]string{"remote_app": s.RemoteApp})
		attrs["env"] = string(env)
	}
	row := SpanRow{
		TraceID: hex.EncodeToString(sum), SpanID: hex.EncodeToString(sum[:8]),
		Name: hook, Start: s.Start, End: end, Service: ServiceOps,
		Model: s.Model, App: appOf(s.Unit), Unit: s.Unit, Hook: hook,
		RawFile: s.RawFile, Attrs: attrs,
	}
	if s.Failed {
		row.StatusCode, row.StatusMsg = "ERROR", "hook failed (status log)"
	}
	return row
}

// TapCoverage is what the unit probes say about where the tap was in place:
// for each unit, the probe sessions that covered it and the tap's
// installation in its charm. It explains why the tap missed a hook.
type TapCoverage struct {
	since time.Time
	units map[string]*unitCover // model + "\x00" + unit
}

type unitCover struct {
	sessions []*coverSession
}

// coverSession is one unit probe session, as one unit saw it.
type coverSession struct {
	host       *hostSession
	events     []coverEvent // loader records about the unit
	installsAt time.Time    // first install or reinstall in this session
}

type hostSession struct {
	start, end  time.Time // hello, last record
	agentStart  time.Time
	unavailAt   time.Time // when the spool couldn't be set up; zero if it could
	unavailWhy  string
	prevEnd     time.Time // the end of the host's previous session
	hasPrevious bool
}

type coverEvent struct {
	ts time.Time
	l  unit.Loader
}

// LoadTapCoverage reads the hello and loader records of every unit probe
// file under raw/unit/. since is when the recording started.
func LoadTapCoverage(root string, since time.Time) (*TapCoverage, error) {
	c := &TapCoverage{since: since, units: map[string]*unitCover{}}
	unitRoot := filepath.Join(NewLayout(root).RawDir(), "unit")
	var paths []string
	err := filepath.WalkDir(unitRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)                              // hourly files of a host, in order
	cur := map[string]*hostSession{}                 // model + host -> its current session
	perUnit := map[string]map[string]*coverSession{} // model + host -> unit -> its part of the current session
	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)
		model, ok := UnitFileModel(rel)
		if !ok {
			continue
		}
		hk := model + "\x00" + hostOf(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var r unit.Record
			if len(line) == 0 || json.Unmarshal(line, &r) != nil {
				continue
			}
			if r.Kind == unit.KindHello && r.Hello != nil {
				hs := &hostSession{start: r.TS, end: r.TS, agentStart: r.Hello.AgentStart}
				if prev := cur[hk]; prev != nil {
					hs.prevEnd, hs.hasPrevious = prev.end, true
				}
				cur[hk], perUnit[hk] = hs, map[string]*coverSession{}
				for _, u := range r.Hello.Units {
					c.session(model, u, hs, perUnit[hk])
				}
				continue
			}
			hs := cur[hk]
			if hs == nil {
				continue // a busy session, or a file cut before its hello
			}
			hs.end = r.TS
			switch {
			case r.Kind == unit.KindLoader && r.Loader != nil && r.Loader.Unit == "":
				if r.Loader.Event == unit.LoaderUnavailable && hs.unavailAt.IsZero() {
					hs.unavailAt, hs.unavailWhy = r.TS, r.Loader.Reason
				}
			case r.Kind == unit.KindLoader && r.Loader != nil:
				cs := c.session(model, r.Loader.Unit, hs, perUnit[hk])
				cs.events = append(cs.events, coverEvent{ts: r.TS, l: *r.Loader})
				if (r.Loader.Event == unit.LoaderInstalled || r.Loader.Event == unit.LoaderReinstalled) && cs.installsAt.IsZero() {
					cs.installsAt = r.TS
				}
			case r.Kind == unit.KindTap && r.Tap != nil:
				c.session(model, r.Tap.Unit, hs, perUnit[hk])
			}
		}
	}
	for _, uc := range c.units {
		sort.SliceStable(uc.sessions, func(i, j int) bool { return uc.sessions[i].host.start.Before(uc.sessions[j].host.start) })
	}
	return c, nil
}

// session returns the unit's part of host session hs, adding it if new.
func (c *TapCoverage) session(model, unitName string, hs *hostSession, perUnit map[string]*coverSession) *coverSession {
	if cs := perUnit[unitName]; cs != nil {
		return cs
	}
	key := model + "\x00" + unitName
	uc := c.units[key]
	if uc == nil {
		uc = &unitCover{}
		c.units[key] = uc
	}
	cs := &coverSession{host: hs}
	uc.sessions = append(uc.sessions, cs)
	perUnit[unitName] = cs
	return cs
}

// Units returns, for each model, the units a unit probe covered, sorted.
func (c *TapCoverage) Units() map[string][]string {
	out := map[string][]string{}
	for key := range c.units {
		model, u, _ := strings.Cut(key, "\x00")
		out[model] = append(out[model], u)
	}
	for _, us := range out {
		sort.Strings(us)
	}
	return out
}

// Covers reports whether a unit probe ever covered the unit.
func (c *TapCoverage) Covers(model, unitName string) bool {
	return c.units[model+"\x00"+unitName] != nil
}

// Reason says why the tap would have missed a hook that started on the unit
// at t: "" when the tap was in place, so the reason is unknown.
func (c *TapCoverage) Reason(model, unitName string, t time.Time) string {
	uc := c.units[model+"\x00"+unitName]
	if uc == nil {
		return ""
	}
	i := sort.Search(len(uc.sessions), func(i int) bool { return uc.sessions[i].host.start.After(t) }) - 1
	if i < 0 {
		if next := uc.sessions[0].host; c.podStarted(next) && !t.Before(next.agentStart) {
			return ReasonPodStart
		}
		return ReasonBeforeProbe
	}
	cs := uc.sessions[i]
	if t.After(cs.host.end) {
		if i+1 == len(uc.sessions) {
			return ReasonAfterProbe
		}
		if next := uc.sessions[i+1].host; c.podStarted(next) && !t.Before(next.agentStart) {
			return ReasonPodStart
		}
		return ReasonNoSession
	}
	if hs := cs.host; !hs.unavailAt.IsZero() && !t.Before(hs.unavailAt) {
		return reasonUnavailPrefix + hs.unavailWhy
	}
	state, why := "", ""
	for _, e := range cs.events {
		if e.ts.After(t) {
			if e.l.Event == unit.LoaderReinstalled && state == unit.LoaderInstalled &&
				!e.l.DirChanged.IsZero() && !t.Before(e.l.DirChanged) {
				return ReasonVenvReplaced
			}
			break
		}
		switch e.l.Event {
		case unit.LoaderInstalled, unit.LoaderReinstalled:
			state = unit.LoaderInstalled
		case unit.LoaderSkipped:
			state, why = unit.LoaderSkipped, e.l.Reason
		case unit.LoaderRemoved:
			state = unit.LoaderRemoved
		}
	}
	switch state {
	case unit.LoaderInstalled:
		return ""
	case unit.LoaderRemoved:
		return ReasonRemoved
	case unit.LoaderSkipped:
		// A charm briefly without its venv, while it is unpacked, gets
		// the tap moments later.
		if !(why == unit.ReasonNoVenv && cs.installsAt.After(t)) {
			if why == unit.ReasonNoVenv {
				return why
			}
			return reasonSkippedPrefix + why
		}
	}
	if c.podStarted(cs.host) && !t.Before(cs.host.agentStart) {
		return ReasonPodStart
	}
	return ReasonNotInstalled
}

// podStarted reports whether the agent of session hs started during the
// recording, after the host's previous session: a pod (re)created by a
// refresh or a deploy, with a fresh charm venv the tap isn't in yet.
func (c *TapCoverage) podStarted(hs *hostSession) bool {
	if hs.agentStart.IsZero() {
		return false
	}
	if hs.hasPrevious {
		return hs.agentStart.After(hs.prevEnd)
	}
	return hs.agentStart.After(c.since)
}

// UntappedReason is the tap_reason of a hook the tap recorded nothing of.
func (c *TapCoverage) UntappedReason(s HookSighting) string {
	if s.Exec == ExecAction && s.Name == "juju-exec" {
		return ReasonJujuExec
	}
	return c.Reason(s.Model, s.Unit, s.Start)
}
