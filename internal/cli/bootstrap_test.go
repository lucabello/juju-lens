package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapHasApps(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	empty := write("empty.json", `{"model":{"name":"m"},"applications":{}}`)
	withApps := write("apps.json", `{"model":{"name":"m"},"applications":{"grafana":{}}}`)
	bad := write("bad.json", `not json`)

	if bootstrapHasApps(filepath.Join(dir, "missing.json")) {
		t.Error("missing file should report no apps")
	}
	if bootstrapHasApps(empty) {
		t.Error("empty applications should report no apps (so it is re-captured)")
	}
	if bootstrapHasApps(bad) {
		t.Error("unparseable file should report no apps")
	}
	if !bootstrapHasApps(withApps) {
		t.Error("file with an application should report apps present")
	}
}
