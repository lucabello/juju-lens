package index

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// InsertUnitResources writes the unit probes' resource samples, disk samples
// and Pebble changes. A Pebble change already indexed (from an earlier
// status) is updated to the later one.
func (d *DB) InsertUnitResources(r recording.UnitResources) error {
	if len(r.Samples)+len(r.Disks)+len(r.Changes) == 0 {
		return nil
	}
	// Models first: the transaction below holds the write lock.
	ids := map[string]int64{}
	for _, m := range modelsOf(r) {
		id, err := d.UpsertModel(m)
		if err != nil {
			return err
		}
		ids[m] = id
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range r.Samples {
		var anon, file, kernel, shmem, high, maxEv, oom, oomKill sql.NullInt64
		if m := s.MemStat; m != nil {
			anon, file = nullInt(m.AnonBytes), nullInt(m.FileBytes)
			kernel, shmem = nullInt(m.KernelBytes), nullInt(m.ShmemBytes)
		}
		if e := s.MemEvents; e != nil {
			high, maxEv, oom, oomKill = nullInt(e.High), nullInt(e.Max), nullInt(e.OOM), nullInt(e.OOMKill)
		}
		_, err := tx.Exec(`INSERT INTO resource_samples(model_id, host, unit, container, ts,
		        cpu_usec, cpu_user_usec, cpu_sys_usec, throttled_usec, mem_bytes, mem_peak_bytes, mem_max_bytes,
		        io_read_bytes, io_write_bytes, cpu_stall_usec, mem_stall_usec, io_stall_usec,
		        mem_anon_bytes, mem_file_bytes, mem_kernel_bytes, mem_shmem_bytes,
		        mem_high_events, mem_max_events, mem_oom_events, mem_oom_kill_events, error)
		    VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), ?, ?, ?, ?, ?,
		        ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`,
			ids[s.Model], s.Host, s.Unit, s.Container, s.TS.UnixNano(),
			s.CPUUsec, s.CPUUserUsec, s.CPUSysUsec, s.ThrottledUsec, s.MemBytes, s.MemPeakBytes, s.MemMaxBytes,
			s.IOReadBytes, s.IOWriteBytes, s.CPUStallUsec, s.MemStallUsec, s.IOStallUsec,
			anon, file, kernel, shmem, high, maxEv, oom, oomKill, s.Error)
		if err != nil {
			return fmt.Errorf("insert resource sample: %w", err)
		}
	}
	for _, s := range r.Disks {
		_, err := tx.Exec(`INSERT INTO disk_samples(model_id, host, unit, ts, path, size_bytes, used_bytes, avail_bytes)
		    VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
			ids[s.Model], s.Host, s.Unit, s.TS.UnixNano(), s.Path, s.SizeBytes, s.UsedBytes, s.AvailBytes)
		if err != nil {
			return fmt.Errorf("insert disk sample: %w", err)
		}
	}
	for _, c := range r.Changes {
		var ready sql.NullInt64
		if !c.ReadyTime.IsZero() {
			ready = sql.NullInt64{Int64: c.ReadyTime.UnixNano(), Valid: true}
		}
		_, err := tx.Exec(`INSERT INTO pebble_changes(model_id, host, unit, container, change_id, kind, summary, status, err, ts_spawn, ts_ready)
		    VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?)
		    ON CONFLICT(model_id, host, container, change_id) DO UPDATE SET
		        status = excluded.status, err = excluded.err, ts_ready = excluded.ts_ready`,
			ids[c.Model], c.Host, c.Unit, c.Container, c.ID, c.Kind, c.Summary, c.Status, c.Err, c.Spawn.UnixNano(), ready)
		if err != nil {
			return fmt.Errorf("insert pebble change: %w", err)
		}
	}
	return tx.Commit()
}

func nullInt(v uint64) sql.NullInt64 { return sql.NullInt64{Int64: int64(v), Valid: true} }

func modelsOf(r recording.UnitResources) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, s := range r.Samples {
		add(s.Model)
	}
	for _, s := range r.Disks {
		add(s.Model)
	}
	for _, c := range r.Changes {
		add(c.Model)
	}
	return out
}

// MemSample is one cgroup's memory at one time, from resource_samples. The
// memory.stat split (Anon, File, Kernel, Shmem) and the memory.events
// counters are only in recordings indexed with them (schema v8); Split and
// Events say whether they were read.
type MemSample struct {
	Container string
	TS        time.Time
	MemBytes  int64
	MaxBytes  int64 // the limit; 0 when unlimited

	Split                     bool
	Anon, File, Kernel, Shmem int64

	Events                                          bool
	HighEvents, MaxEvents, OOMEvents, OOMKillEvents int64
}

// memStatCols and memEventCols are the resource_samples columns schema v8
// added.
var (
	memStatCols  = []string{"mem_anon_bytes", "mem_file_bytes", "mem_kernel_bytes", "mem_shmem_bytes"}
	memEventCols = []string{"mem_high_events", "mem_max_events", "mem_oom_events", "mem_oom_kill_events"}
)

// hasColumns reports whether table has every one of cols.
func (d *DB) hasColumns(table string, cols []string) bool {
	rows, err := d.sql.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			have[n] = true
		}
	}
	for _, c := range cols {
		if !have[c] {
			return false
		}
	}
	return true
}

// MemorySamples returns the memory of every cgroup a unit probe sampled on
// host between from and to, the probe's own excluded, in time order.
func (d *DB) MemorySamples(modelID int64, host string, from, to time.Time) ([]MemSample, error) {
	split := d.hasColumns("resource_samples", memStatCols)
	events := d.hasColumns("resource_samples", memEventCols)
	cols := "container, ts, mem_bytes, COALESCE(mem_max_bytes, 0)"
	// A row from before the probe read memory.stat or memory.events has
	// the columns NULL.
	if split {
		cols += ", " + memStatCols[0] + " IS NOT NULL, " + coalesced(memStatCols)
	}
	if events {
		cols += ", " + memEventCols[0] + " IS NOT NULL, " + coalesced(memEventCols)
	}
	rows, err := d.sql.Query(`SELECT `+cols+` FROM resource_samples
	    WHERE model_id = ? AND host = ? AND container != ? AND ts BETWEEN ? AND ? AND error IS NULL
	    ORDER BY ts, container`,
		modelID, host, recording.ProbeContainer, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemSample
	for rows.Next() {
		var (
			s  MemSample
			ts int64
		)
		dest := []any{&s.Container, &ts, &s.MemBytes, &s.MaxBytes}
		if split {
			dest = append(dest, &s.Split, &s.Anon, &s.File, &s.Kernel, &s.Shmem)
		}
		if events {
			dest = append(dest, &s.Events, &s.HighEvents, &s.MaxEvents, &s.OOMEvents, &s.OOMKillEvents)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		s.TS = time.Unix(0, ts).UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

func coalesced(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = "COALESCE(" + c + ", 0)"
	}
	return strings.Join(out, ", ")
}

// PebbleChange is a workload Pebble change as indexed: at the latest status
// the probe saw. Ready is zero while the change wasn't ready.
type PebbleChange struct {
	Container, ID, Kind, Summary, Status, Err string
	Spawn, Ready                              time.Time
}

// PebbleChanges returns the Pebble changes on host that were spawned
// between from and to, by spawn time.
func (d *DB) PebbleChanges(modelID int64, host string, from, to time.Time) ([]PebbleChange, error) {
	rows, err := d.sql.Query(`SELECT container, change_id, kind, summary, status, COALESCE(err, ''), ts_spawn, ts_ready
	    FROM pebble_changes WHERE model_id = ? AND host = ? AND ts_spawn BETWEEN ? AND ?
	    ORDER BY ts_spawn, container, change_id`,
		modelID, host, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PebbleChange
	for rows.Next() {
		var (
			c     PebbleChange
			spawn int64
			ready sql.NullInt64
		)
		if err := rows.Scan(&c.Container, &c.ID, &c.Kind, &c.Summary, &c.Status, &c.Err, &spawn, &ready); err != nil {
			return nil, err
		}
		c.Spawn = time.Unix(0, spawn).UTC()
		if ready.Valid {
			c.Ready = time.Unix(0, ready.Int64).UTC()
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
