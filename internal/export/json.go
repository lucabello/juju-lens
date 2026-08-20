package export

import (
	"encoding/json"
	"io"
)

// WriteJSON marshals r as an indented JSON document — the "feed to a script
// or an MCP-style tool" format. Field names are snake_case to match
// manifest.json's own convention.
func WriteJSON(w io.Writer, r *Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}
