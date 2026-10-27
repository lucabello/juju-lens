package index

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// InsertProfile stores the stack samples of one hook process, replacing any
// earlier copy of the same process. Its stacks reference the ops spans by
// id; the spans themselves are inserted with InsertSpan.
func (d *DB) InsertProfile(p recording.OpsProfile) error {
	modelID, err := d.UpsertModel(p.Model)
	if err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range []string{"profile_samples", "profile_stacks", "profile_frames", "profiles"} {
		if _, err := tx.Exec("DELETE FROM "+t+" WHERE root_span_id = ?", p.RootSpanID); err != nil {
			return fmt.Errorf("replacing profile %s: %w", p.RootSpanID, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO profiles(root_span_id, model_id, unit, ts_start, interval_ns, samples, dropped,
		                      rss_start, rss_peak, sampler_cpu_ns, complete)
		 VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.RootSpanID, modelID, p.Unit, p.Start.UnixNano(), p.IntervalNs, p.Samples, p.Dropped,
		p.RSSStart, p.RSSPeak, p.SamplerCPUNs, p.Complete,
	); err != nil {
		return err
	}
	if err := insertRows(tx, `INSERT INTO profile_frames(root_span_id, frame_id, file, func, line) VALUES (?, ?, ?, ?, ?)`,
		len(p.Frames), func(i int) []any {
			f := p.Frames[i]
			return []any{p.RootSpanID, i, f.File, f.Func, f.Line}
		}); err != nil {
		return err
	}
	if err := insertRows(tx, `INSERT INTO profile_stacks(root_span_id, stack_id, span_id, frames_json,
		                                                 samples, wall_ns, cpu_ns, mem_bytes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		len(p.Stacks), func(i int) []any {
			s := p.Stacks[i]
			frames, _ := json.Marshal(s.Frames)
			if s.Frames == nil {
				frames = []byte("[]")
			}
			return []any{p.RootSpanID, i, s.SpanID, string(frames), s.Samples, s.WallNs, s.CPUNs, s.MemBytes}
		}); err != nil {
		return err
	}
	if err := insertRows(tx, `INSERT INTO profile_samples(root_span_id, seq, ts, stack_id, rss_bytes)
		 VALUES (?, ?, ?, ?, ?)`,
		len(p.Timeline), func(i int) []any {
			s := p.Timeline[i]
			var stack sql.NullInt64
			if s.Stack >= 0 {
				stack = sql.NullInt64{Int64: int64(s.Stack), Valid: true}
			}
			return []any{p.RootSpanID, i, s.Time.UnixNano(), stack, s.RSSBytes}
		}); err != nil {
		return err
	}
	return tx.Commit()
}

// insertRows runs one prepared insert n times, with the arguments args
// gives for each row.
func insertRows(tx *sql.Tx, query string, n int, args func(int) []any) error {
	if n == 0 {
		return nil
	}
	st, err := tx.Prepare(query)
	if err != nil {
		return err
	}
	defer st.Close()
	for i := range n {
		if _, err := st.Exec(args(i)...); err != nil {
			return err
		}
	}
	return nil
}

// Profiles reads back the stack samples of the hook processes whose ops root
// spans are given, in that order. A root with no profile is skipped.
func (d *DB) Profiles(rootSpanIDs []string) ([]recording.OpsProfile, error) {
	var out []recording.OpsProfile
	for _, id := range rootSpanIDs {
		p, ok, err := d.profile(id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (d *DB) profile(root string) (recording.OpsProfile, bool, error) {
	p := recording.OpsProfile{RootSpanID: root}
	var (
		start int64
		unit  sql.NullString
		model sql.NullString
	)
	err := d.sql.QueryRow(
		`SELECT p.ts_start, p.interval_ns, p.samples, p.dropped, p.rss_start, p.rss_peak,
		        p.sampler_cpu_ns, p.complete, p.unit, m.name
		   FROM profiles p LEFT JOIN models m ON m.id = p.model_id
		  WHERE p.root_span_id = ?`, root).
		Scan(&start, &p.IntervalNs, &p.Samples, &p.Dropped, &p.RSSStart, &p.RSSPeak,
			&p.SamplerCPUNs, &p.Complete, &unit, &model)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.Start = time.Unix(0, start).UTC()
	p.Unit, p.Model = unit.String, model.String

	rows, err := d.sql.Query(`SELECT file, func, line FROM profile_frames WHERE root_span_id = ? ORDER BY frame_id`, root)
	if err != nil {
		return p, false, err
	}
	for rows.Next() {
		var f recording.ProfileFrame
		if err := rows.Scan(&f.File, &f.Func, &f.Line); err != nil {
			rows.Close()
			return p, false, err
		}
		p.Frames = append(p.Frames, f)
	}
	rows.Close()

	rows, err = d.sql.Query(`SELECT span_id, frames_json, samples, wall_ns, cpu_ns, mem_bytes
	                           FROM profile_stacks WHERE root_span_id = ? ORDER BY stack_id`, root)
	if err != nil {
		return p, false, err
	}
	for rows.Next() {
		var (
			s      recording.ProfileStack
			frames string
		)
		if err := rows.Scan(&s.SpanID, &frames, &s.Samples, &s.WallNs, &s.CPUNs, &s.MemBytes); err != nil {
			rows.Close()
			return p, false, err
		}
		_ = json.Unmarshal([]byte(frames), &s.Frames)
		p.Stacks = append(p.Stacks, s)
	}
	rows.Close()

	rows, err = d.sql.Query(`SELECT ts, stack_id, rss_bytes FROM profile_samples WHERE root_span_id = ? ORDER BY seq`, root)
	if err != nil {
		return p, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ts    int64
			stack sql.NullInt64
			s     recording.ProfileSample
		)
		if err := rows.Scan(&ts, &stack, &s.RSSBytes); err != nil {
			return p, false, err
		}
		s.Time = time.Unix(0, ts).UTC()
		s.Stack = -1
		if stack.Valid {
			s.Stack = int(stack.Int64)
		}
		p.Timeline = append(p.Timeline, s)
	}
	return p, true, rows.Err()
}

// SpanHost is the unit probe host (pod or machine, raw/unit/<model>/<host>/)
// an ops span was recorded on, or "" for a span from elsewhere.
func (d *DB) SpanHost(spanID string) (string, error) {
	var raw sql.NullString
	err := d.sql.QueryRow(`SELECT raw_file FROM spans WHERE span_id = ?`, spanID).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	parts := strings.Split(raw.String, "/")
	if len(parts) < 5 || parts[0] != "raw" || parts[1] != "unit" {
		return "", nil
	}
	return parts[3], nil
}
