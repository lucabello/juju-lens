// Package index is the SQLite-backed query engine for the viewer. The DB is
// rebuildable from a recording's raw/ directory, so it is safe to delete
// and re-create at any time; nothing critical lives only in the index.
//
// M2 keeps the schema focussed on what the viewer needs today: spans,
// models, and snapshots. Log records, inventory, and diffs join in later
// milestones.
package index

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"

	_ "modernc.org/sqlite"
)

// SchemaVersion is bumped whenever the SQL below changes shape. The rebuild
// command uses it to decide whether an existing DB can be reused.
const SchemaVersion = 3

// The full schema. Statements are executed in order.
var schema = []string{
	`PRAGMA journal_mode = WAL`,
	`PRAGMA synchronous = NORMAL`,
	`PRAGMA foreign_keys = ON`,

	`CREATE TABLE IF NOT EXISTS meta (
	    key   TEXT PRIMARY KEY,
	    value TEXT NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS models (
	    id   INTEGER PRIMARY KEY AUTOINCREMENT,
	    name TEXT NOT NULL UNIQUE,
	    uuid TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS spans (
	    span_id        TEXT PRIMARY KEY,   -- hex
	    trace_id       TEXT NOT NULL,
	    parent_span_id TEXT,
	    model_id       INTEGER REFERENCES models(id),
	    ts_start       INTEGER NOT NULL,   -- unix nano
	    ts_end         INTEGER NOT NULL,
	    name           TEXT NOT NULL,
	    service        TEXT,
	    unit           TEXT,
	    app            TEXT,               -- derived from unit ("grafana/0" -> "grafana")
	    hook           TEXT,
	    relation       TEXT,
	    status         TEXT,
	    status_msg     TEXT,
	    raw_file       TEXT,               -- calls-*.jsonl the request came from
	    raw_offset     INTEGER,            -- byte offset of the request line
	    attrs_json     TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS spans_by_start ON spans(ts_start)`,
	`CREATE INDEX IF NOT EXISTS spans_by_model ON spans(model_id, ts_start)`,
	`CREATE INDEX IF NOT EXISTS spans_by_trace ON spans(trace_id)`,
	`CREATE INDEX IF NOT EXISTS spans_by_parent ON spans(parent_span_id)`,
	`CREATE INDEX IF NOT EXISTS spans_by_unit ON spans(unit, ts_start)`,

	// Snapshots hold viewer-derived point-in-time state (app-status,
	// unit-status, databag). M2 only writes app-status and unit-status
	// scopes; the table shape is stable so M4 can add more. producing_span_id
	// is intentionally not a hard FK because incremental indexers may
	// insert a snapshot before its producing span row is committed.
	`CREATE TABLE IF NOT EXISTS snapshots (
	    id                INTEGER PRIMARY KEY AUTOINCREMENT,
	    model_id          INTEGER REFERENCES models(id),
	    ts                INTEGER NOT NULL,
	    kind              TEXT NOT NULL,   -- 'app-status', 'unit-status', 'databag', ...
	    scope             TEXT NOT NULL,   -- e.g. 'app-status:grafana' or 'unit-status:grafana/0'
	    body_json         TEXT NOT NULL,
	    producing_span_id TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS snap_by_scope_ts ON snapshots(model_id, scope, ts)`,
	`CREATE INDEX IF NOT EXISTS snap_by_kind_ts  ON snapshots(model_id, kind, ts)`,

	// Log records ingested from `juju debug-log` (M3). Joined to spans by
	// span_id when a line carries one, else by (unit, ts window). Raw text lines
	// remain the source of truth under raw/juju/; this table is rebuildable.
	`CREATE TABLE IF NOT EXISTS log_records (
	    id         INTEGER PRIMARY KEY AUTOINCREMENT,
	    model_id   INTEGER REFERENCES models(id),
	    ts         INTEGER NOT NULL,   -- unix nano
	    source     TEXT NOT NULL,      -- 'debug-log'
	    entity     TEXT,               -- raw juju entity tag
	    unit       TEXT,               -- 'grafana/0' when the entity is a unit
	    level      TEXT,
	    module     TEXT,
	    body       TEXT NOT NULL,
	    trace_id   TEXT,
	    span_id    TEXT,
	    raw_file   TEXT,
	    raw_line   INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS log_by_ts       ON log_records(model_id, ts)`,
	`CREATE INDEX IF NOT EXISTS log_by_unit_ts  ON log_records(model_id, unit, ts)`,
	`CREATE INDEX IF NOT EXISTS log_by_span     ON log_records(span_id)`,
}

// DB is a thin wrapper around *sql.DB carrying convenience methods for the
// viewer.
type DB struct {
	sql *sql.DB
}

// Open opens an existing index or creates a new one at path. It sets pragmas
// and runs the schema.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// SQLite drivers do not always honour a single connection cleanly for
	// WAL databases; keep it small but non-zero.
	db.SetMaxOpenConns(4)
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("schema stmt %q: %w", firstLine(stmt), err)
		}
	}
	d := &DB{sql: db}
	if err := d.setMeta("schema_version", fmt.Sprintf("%d", SchemaVersion)); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the underlying database handle.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the wrapped database. Prefer the typed helpers below when
// possible; SQL is here so tests and later milestones can add queries
// without expanding this package.
func (d *DB) SQL() *sql.DB { return d.sql }

// UpsertModel returns the id of a model row, inserting one if needed.
func (d *DB) UpsertModel(name string) (int64, error) {
	return d.UpsertModelWithUUID(name, "")
}

// UpsertModelWithUUID is UpsertModel that also records the model's UUID,
// backfilling it on an existing row whose uuid is still NULL (the first span
// carrying it wins). A blank uuid is ignored so callers without one are safe.
func (d *DB) UpsertModelWithUUID(name, uuid string) (int64, error) {
	if name == "" {
		name = "default"
	}
	// Fast path: try to fetch the id first.
	var id int64
	var existing sql.NullString
	err := d.sql.QueryRow(`SELECT id, uuid FROM models WHERE name = ?`, name).Scan(&id, &existing)
	if err == nil {
		if uuid != "" && !existing.Valid {
			if _, uerr := d.sql.Exec(`UPDATE models SET uuid = ? WHERE id = ?`, uuid, id); uerr != nil {
				return 0, uerr
			}
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if uuid != "" {
		res, err := d.sql.Exec(`INSERT INTO models(name, uuid) VALUES (?, ?)`, name, uuid)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := d.sql.Exec(`INSERT INTO models(name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertSpan inserts a SpanRow. Attributes are stored as a JSON object and
// the model_id is looked up (creating it if necessary) from row.Model. On
// duplicate span_id the row is replaced.
func (d *DB) InsertSpan(row recording.SpanRow) error {
	modelID, err := d.UpsertModelWithUUID(row.Model, row.ModelUUID)
	if err != nil {
		return err
	}
	attrs, err := json.Marshal(row.Attrs)
	if err != nil {
		return fmt.Errorf("marshal attrs: %w", err)
	}
	_, err = d.sql.Exec(
		`INSERT INTO spans(span_id, trace_id, parent_span_id, model_id,
		                   ts_start, ts_end, name, service, unit, app,
		                   hook, relation, status, status_msg,
		                   raw_file, raw_offset, attrs_json)
		 VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''),
		         NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
		         NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)
		 ON CONFLICT(span_id) DO UPDATE SET
		     trace_id = excluded.trace_id,
		     parent_span_id = excluded.parent_span_id,
		     model_id = excluded.model_id,
		     ts_start = excluded.ts_start,
		     ts_end = excluded.ts_end,
		     name = excluded.name,
		     service = excluded.service,
		     unit = excluded.unit,
		     app = excluded.app,
		     hook = excluded.hook,
		     relation = excluded.relation,
		     status = excluded.status,
		     status_msg = excluded.status_msg,
		     raw_file = excluded.raw_file,
		     raw_offset = excluded.raw_offset,
		     attrs_json = excluded.attrs_json`,
		row.SpanID, row.TraceID, row.ParentSpanID, modelID,
		row.Start.UnixNano(), row.End.UnixNano(),
		row.Name, row.Service, row.Unit, appOf(row.Unit),
		row.Hook, row.Relation, row.StatusCode, row.StatusMsg,
		row.RawFile, row.RawOffset, string(attrs),
	)
	return err
}

// InsertSnapshot appends a snapshot row.
func (d *DB) InsertSnapshot(model string, ts time.Time, kind, scope, bodyJSON, producingSpanID string) error {
	modelID, err := d.UpsertModel(model)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(
		`INSERT INTO snapshots(model_id, ts, kind, scope, body_json, producing_span_id)
		 VALUES (?, ?, ?, ?, ?, NULLIF(?, ''))`,
		modelID, ts.UnixNano(), kind, scope, bodyJSON, producingSpanID,
	)
	return err
}

// InsertLog appends one parsed debug-log record.
func (d *DB) InsertLog(rec recording.LogRecord) error {
	modelID, err := d.UpsertModel(rec.Model)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(
		`INSERT INTO log_records(model_id, ts, source, entity, unit, level, module,
		                         body, trace_id, span_id, raw_file, raw_line)
		 VALUES (?, ?, 'debug-log', NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
		         NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`,
		modelID, rec.Ts.UnixNano(), rec.Entity, rec.Unit, rec.Level, rec.Module,
		rec.Message, rec.TraceID, rec.SpanID, rec.RawFile, rec.RawLine,
	)
	return err
}

// LogRow is one row from log_records returned to the viewer.
type LogRow struct {
	Ts      time.Time
	Unit    string
	Level   string
	Module  string
	Body    string
	SpanID  string
	Matched string // why this log correlated: "span" (exact) or "window" (fuzzy)
}

// LogsForSpan returns the log records correlated to a span, per the §7 join:
// any line carrying the span's span_id (exact match), plus lines from the same
// unit within [start-window, end+window] (fuzzy fallback). Results are ordered
// by time and de-duplicated, capped at limit rows. A zero modelID searches all
// models.
func (d *DB) LogsForSpan(modelID int64, spanID, unit string, start, end time.Time, window time.Duration, limit int) ([]LogRow, error) {
	lo := start.Add(-window).UnixNano()
	hi := end.Add(window).UnixNano()
	// One query with an OR keeps ordering and de-dup simple: match the exact
	// span_id, or the same unit inside the time window.
	q := `SELECT ts, COALESCE(unit,''), COALESCE(level,''), COALESCE(module,''),
	             body, COALESCE(span_id,''),
	             CASE WHEN span_id = ? AND ? <> '' THEN 'span' ELSE 'window' END
	        FROM log_records
	       WHERE (? = 0 OR model_id = ?)
	         AND ( (span_id = ? AND ? <> '')
	            OR (unit = ? AND ? <> '' AND ts BETWEEN ? AND ?) )
	       ORDER BY ts
	       LIMIT ?`
	rows, err := d.sql.Query(q,
		spanID, spanID,
		modelID, modelID,
		spanID, spanID,
		unit, unit, lo, hi,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRow
	for rows.Next() {
		var (
			r    LogRow
			nano int64
		)
		if err := rows.Scan(&nano, &r.Unit, &r.Level, &r.Module, &r.Body, &r.SpanID, &r.Matched); err != nil {
			return nil, err
		}
		r.Ts = time.Unix(0, nano).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// SnapshotsByProducingSpan returns the snapshots a given span produced, of the
// requested kind, newest scope first. It backs the M4 databag diff: for a
// CommitHookChanges span it yields the databags that hook wrote.
func (d *DB) SnapshotsByProducingSpan(spanID, kind string) ([]SnapshotRow, error) {
	rows, err := d.sql.Query(
		`SELECT scope, body_json, ts FROM snapshots
		  WHERE producing_span_id = ? AND kind = ?
		  ORDER BY scope`, spanID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var (
			r    SnapshotRow
			nano int64
		)
		if err := rows.Scan(&r.Scope, &r.Body, &nano); err != nil {
			return nil, err
		}
		r.Ts = time.Unix(0, nano).UTC()
		r.Kind = kind
		out = append(out, r)
	}
	return out, rows.Err()
}

// PrevSnapshotBefore returns the snapshot body for scope strictly before ts —
// the value that was in effect just prior to a write at ts. Empty when none.
func (d *DB) PrevSnapshotBefore(modelID int64, scope string, ts time.Time) (string, error) {
	var body string
	err := d.sql.QueryRow(
		`SELECT body_json FROM snapshots
		  WHERE model_id = ? AND scope = ? AND ts < ?
		  ORDER BY ts DESC LIMIT 1`,
		modelID, scope, ts.UnixNano()).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body, err
}

// LogCount returns the total number of log records in the DB.
func (d *DB) LogCount() (int64, error) {
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM log_records`).Scan(&n)
	return n, err
}

// Model represents a row in the models table.
type Model struct {
	ID   int64
	Name string
	UUID string
}

// Models returns every known model, ordered by name.
func (d *DB) Models() ([]Model, error) {
	rows, err := d.sql.Query(`SELECT id, name, COALESCE(uuid,'') FROM models ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Name, &m.UUID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Spans returns every span in the given model, ordered by start time.
// Passing 0 as modelID returns all spans across all models.
func (d *DB) Spans(modelID int64) ([]recording.SpanRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if modelID == 0 {
		rows, err = d.sql.Query(
			`SELECT s.span_id, s.trace_id, COALESCE(s.parent_span_id,''),
			        s.ts_start, s.ts_end, s.name,
			        COALESCE(s.service,''), COALESCE(s.unit,''),
			        COALESCE(s.hook,''), COALESCE(s.relation,''),
			        COALESCE(s.status,''), COALESCE(s.status_msg,''),
			        s.attrs_json, COALESCE(m.name,'')
			   FROM spans s
			   LEFT JOIN models m ON m.id = s.model_id
			  ORDER BY s.ts_start`)
	} else {
		rows, err = d.sql.Query(
			`SELECT s.span_id, s.trace_id, COALESCE(s.parent_span_id,''),
			        s.ts_start, s.ts_end, s.name,
			        COALESCE(s.service,''), COALESCE(s.unit,''),
			        COALESCE(s.hook,''), COALESCE(s.relation,''),
			        COALESCE(s.status,''), COALESCE(s.status_msg,''),
			        s.attrs_json, COALESCE(m.name,'')
			   FROM spans s
			   LEFT JOIN models m ON m.id = s.model_id
			  WHERE s.model_id = ?
			  ORDER BY s.ts_start`, modelID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recording.SpanRow
	for rows.Next() {
		var (
			startNano, endNano int64
			attrsJSON          string
			row                recording.SpanRow
		)
		if err := rows.Scan(
			&row.SpanID, &row.TraceID, &row.ParentSpanID,
			&startNano, &endNano, &row.Name,
			&row.Service, &row.Unit,
			&row.Hook, &row.Relation,
			&row.StatusCode, &row.StatusMsg,
			&attrsJSON, &row.Model,
		); err != nil {
			return nil, err
		}
		row.Start = time.Unix(0, startNano).UTC()
		row.End = time.Unix(0, endNano).UTC()
		row.Attrs = map[string]string{}
		if attrsJSON != "" {
			_ = json.Unmarshal([]byte(attrsJSON), &row.Attrs)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// LatestSnapshotBefore returns the newest snapshot body for scope at or
// before ts, or "" when none exists.
func (d *DB) LatestSnapshotBefore(modelID int64, scope string, ts time.Time) (string, error) {
	var body string
	err := d.sql.QueryRow(
		`SELECT body_json FROM snapshots
		  WHERE model_id = ? AND scope = ? AND ts <= ?
		  ORDER BY ts DESC LIMIT 1`,
		modelID, scope, ts.UnixNano()).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body, err
}

// SnapshotRow is one row from the snapshots table returned to viewers.
type SnapshotRow struct {
	Kind  string
	Scope string
	Body  string
	Ts    time.Time
}

// LatestPerScope returns the most recent snapshot per scope for the given
// kind. Useful for the M2 status pane's "latest known" view.
func (d *DB) LatestPerScope(modelID int64, kind string) ([]SnapshotRow, error) {
	rows, err := d.sql.Query(
		`SELECT s.scope, s.body_json, s.ts FROM snapshots s
		  WHERE s.model_id = ? AND s.kind = ? AND s.ts = (
		      SELECT MAX(t.ts) FROM snapshots t
		       WHERE t.model_id = s.model_id
		         AND t.kind     = s.kind
		         AND t.scope    = s.scope)
		  ORDER BY s.scope`,
		modelID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var (
			r    SnapshotRow
			nano int64
		)
		if err := rows.Scan(&r.Scope, &r.Body, &nano); err != nil {
			return nil, err
		}
		r.Ts = time.Unix(0, nano).UTC()
		r.Kind = kind
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestPerScopeAsOf is LatestPerScope restricted to snapshots at or before ts:
// the value each scope held at instant ts. This backs the M4 point-in-time
// Status pane ("what would juju status have shown here?"), walking backward from
// the cursor to the nearest snapshot per scope.
func (d *DB) LatestPerScopeAsOf(modelID int64, kind string, ts time.Time) ([]SnapshotRow, error) {
	rows, err := d.sql.Query(
		`SELECT s.scope, s.body_json, s.ts FROM snapshots s
		  WHERE s.model_id = ? AND s.kind = ? AND s.ts <= ? AND s.ts = (
		      SELECT MAX(t.ts) FROM snapshots t
		       WHERE t.model_id = s.model_id
		         AND t.kind     = s.kind
		         AND t.scope    = s.scope
		         AND t.ts      <= ?)
		  ORDER BY s.scope`,
		modelID, kind, ts.UnixNano(), ts.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var (
			r    SnapshotRow
			nano int64
		)
		if err := rows.Scan(&r.Scope, &r.Body, &nano); err != nil {
			return nil, err
		}
		r.Ts = time.Unix(0, nano).UTC()
		r.Kind = kind
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpanCount returns the total number of spans in the DB. Handy for status
// summaries.
func (d *DB) SpanCount() (int64, error) {
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&n)
	return n, err
}

// Reset deletes all indexed data (spans, snapshots, logs, models) while leaving
// the schema and file in place. It lets a rebuild happen on the same inode, so
// a `view --follow` process with the DB open sees the refresh instead of being
// stranded on an unlinked file. Use os.Remove + Open instead when the schema
// itself may have changed.
func (d *DB) Reset() error {
	for _, t := range []string{"log_records", "snapshots", "spans", "models"} {
		if _, err := d.sql.Exec("DELETE FROM " + t); err != nil {
			return fmt.Errorf("resetting %s: %w", t, err)
		}
	}
	return nil
}

func (d *DB) setMeta(key, value string) error {
	_, err := d.sql.Exec(
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// appOf turns "grafana/0" into "grafana"; returns the input on strings that
// aren't unit names.
func appOf(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
