package cli

import "testing"

func TestParseRenames(t *testing.T) {
	m, err := parseRenames([]string{"uuid-1=jubilant", "old=new"})
	if err != nil {
		t.Fatalf("parseRenames: %v", err)
	}
	if m["uuid-1"] != "jubilant" || m["old"] != "new" {
		t.Fatalf("unexpected map: %v", m)
	}
	if got, err := parseRenames(nil); err != nil || got != nil {
		t.Fatalf("nil input: got %v, %v", got, err)
	}
	for _, bad := range []string{"noeq", "=new", "old=", ""} {
		if _, err := parseRenames([]string{bad}); err == nil {
			t.Errorf("parseRenames(%q) = nil error, want error", bad)
		}
	}
}

func TestRenameModel(t *testing.T) {
	renames := map[string]string{"uuid-1": "jubilant"}
	if got := renameModel(renames, "uuid-1"); got != "jubilant" {
		t.Errorf("renameModel mapped = %q, want jubilant", got)
	}
	if got := renameModel(renames, "other"); got != "other" {
		t.Errorf("renameModel passthrough = %q, want other", got)
	}
	if got := renameModel(nil, "x"); got != "x" {
		t.Errorf("renameModel nil table = %q, want x", got)
	}
}
