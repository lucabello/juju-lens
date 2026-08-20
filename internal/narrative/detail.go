package narrative

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file holds the format-agnostic data-gathering and JSON/diff logic
// behind a hook's detail view — what internal/viewer's inspector overlay and
// internal/export's per-event detail both need: which databags/config a hook
// touched, and a structural (git-style) diff between the before/after
// values. Nothing here renders to a terminal or a document; that's each
// caller's job (lipgloss styling in internal/viewer, Markdown/JSON in
// internal/export).

// IsConfigChanged reports whether an event is a unit's config-changed hook,
// for which a detail view should show the charm-config diff.
func IsConfigChanged(ev Event) bool {
	return ev.Kind == EvHook && ev.Summary == "config-changed"
}

// Inspectable reports whether an event has anything worth drilling into: a
// databag change, a config change, a failure, or a status the hook drove the
// charm into (M10).
func Inspectable(ev Event) bool {
	return ev.HasDatabag || IsConfigChanged(ev) || ev.Fail != FailNone || len(ev.Statuses) > 0
}

// DatabagEntry is one row under a relation: the "app"/"unit (name)" label and
// the scope its body lives at.
type DatabagEntry struct {
	Label string
	Scope string
}

// LocalDatabagEntries lists the local application's databags on a relation —
// its application databag first, then each of its units' databags (sorted) —
// from the set of databag scopes present at the event.
func LocalDatabagEntries(body map[string]string, key, local string) []DatabagEntry {
	var entries []DatabagEntry
	if _, ok := body["databag:"+key+":"+local]; ok {
		entries = append(entries, DatabagEntry{"app", "databag:" + key + ":" + local})
	}
	var units []string
	for scope := range body {
		k, entity := SplitDatabagScope(scope)
		if k == key && appOf(entity) == local && strings.ContainsRune(entity, '/') {
			units = append(units, entity)
		}
	}
	sort.Strings(units)
	for _, u := range units {
		entries = append(entries, DatabagEntry{"unit (" + u + ")", "databag:" + key + ":" + u})
	}
	return entries
}

// SplitDatabagScope splits "databag:<key>:<entity>" into its relation key and
// entity (unit or application name).
func SplitDatabagScope(scope string) (key, entity string) {
	s := strings.TrimPrefix(scope, "databag:")
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// FormatDatabagLabel turns a databag scope into a readable relation label and
// reports whether it is the application databag (vs a unit databag):
//
//	databag:loki.certificates#ca.certificates:loki/0 -> "loki:certificates → ca:certificates", false
//	databag:mimir.mimir-peers:mimir/0                -> "mimir:mimir-peers (peer)", false
func FormatDatabagLabel(scope string) (rel string, isApp bool) {
	key, entity := SplitDatabagScope(scope)
	return RelationLabel(key, appOf(entity)), !strings.ContainsRune(entity, '/')
}

// RelationLabel renders "app:endpoint → app:endpoint" (or "app:endpoint
// (peer)") with the local application placed first, plain text (no
// colouring — internal/viewer wraps its own coloured variant around the same
// CutDot/local-side-first logic).
func RelationLabel(key, local string) string {
	seg := func(app, ep string) string { return app + ":" + ep }
	if before, after, ok := strings.Cut(key, "#"); ok {
		a1, e1 := CutDot(before)
		a2, e2 := CutDot(after)
		if a2 == local && a1 != local { // put the local side first
			a1, e1, a2, e2 = a2, e2, a1, e1
		}
		return seg(a1, e1) + " → " + seg(a2, e2)
	}
	a, e := CutDot(key)
	return seg(a, e) + " (peer)"
}

// CutDot splits "app.endpoint" into its two parts.
func CutDot(s string) (app, ep string) {
	if a, b, ok := strings.Cut(s, "."); ok {
		return a, b
	}
	return s, ""
}

// DiffLine is one line of a git-style diff: Op is ' ' (context), '+' or '-'.
type DiffLine struct {
	Op   byte
	Text string
}

// MarshalJSON renders Op as a single-character string ("+"/"-"/" ") instead
// of its raw byte value, so export's JSON output reads naturally.
func (d DiffLine) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Op   string `json:"op"`
		Text string `json:"text"`
	}{Op: string(d.Op), Text: d.Text})
}

// LineDiff computes a minimal line diff between a and b via an LCS table.
// Inputs are small (a pretty-printed databag/config), so the O(n·m) table is
// fine.
func LineDiff(a, b []string) []DiffLine {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{' ', a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{'-', a[i]})
			i++
		default:
			out = append(out, DiffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{'+', b[j]})
	}
	return out
}

// SplitLines splits pretty-printed JSON into lines for LineDiff, treating an
// empty string as no lines rather than one empty line.
func SplitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// PrettyJSON renders a JSON body as indented, recursively-expanded JSON: any
// string field whose value is itself JSON — or nested YAML — is parsed and
// expanded in place, so a databag holding `{"alert_rules":"{...}"}` or a
// YAML relation-state blob reads as structure rather than an escaped string.
// Object keys are sorted, so two versions diff cleanly. Returns the input
// unchanged when it isn't JSON.
func PrettyJSON(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	out, err := json.MarshalIndent(ExpandJSON([]byte(body)), "", "  ")
	if err != nil {
		return body
	}
	return string(out)
}

// ExpandJSON decodes raw and recursively expands any string leaf that is
// itself JSON or YAML. Non-JSON input is returned as a plain string.
func ExpandJSON(raw []byte) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return expandValue(v)
}

func expandValue(v any) any {
	switch t := v.(type) {
	case string:
		if e, ok := expandString(t); ok {
			return e
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = expandValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = expandValue(val)
		}
		return t
	}
	return v
}

// expandString tries to interpret a leaf string as embedded structure: JSON when
// it opens with {/[, else multi-line YAML (Juju stores relation-state and some
// databag values as YAML). It only expands when the result is a non-empty
// mapping or sequence — plain scalars (numbers, URLs, addresses) are left as-is.
func expandString(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, false
	}
	if t[0] == '{' || t[0] == '[' {
		var inner any
		if json.Unmarshal([]byte(t), &inner) == nil {
			return expandValue(inner), true
		}
	}
	if strings.Contains(t, "\n") {
		var inner any
		if yaml.Unmarshal([]byte(s), &inner) == nil {
			if norm := normalizeYAML(inner); isStructured(norm) {
				return expandValue(norm), true
			}
		}
	}
	return nil, false
}

// isStructured reports whether v is a non-empty map or slice (as opposed to a
// scalar we should leave untouched).
func isStructured(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return false
}

// normalizeYAML converts YAML's map shapes into JSON-marshalable
// map[string]any recursively (yaml.v3 already uses string keys for interface
// targets, but be defensive about map[any]any from older shapes).
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeYAML(val)
		}
		return t
	case map[any]any:
		mm := make(map[string]any, len(t))
		for k, val := range t {
			mm[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return mm
	case []any:
		for i, val := range t {
			t[i] = normalizeYAML(val)
		}
		return t
	}
	return v
}

// MarshalIndent renders v as indented JSON, or "" on error.
func MarshalIndent(v any) string {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}
