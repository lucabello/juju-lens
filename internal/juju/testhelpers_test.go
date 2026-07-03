package juju

import (
	"os"
	"testing"
)

// writeExecutable drops a shell script at path with 0755 perms.
func writeExecutable(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o755)
}

// writeArgvShim writes a shim binary that appends its argv to argFile and
// prints "{}\n" so callers that expect JSON succeed.
func writeArgvShim(path, argFile string) error {
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> " + argFile + "\n" +
		"echo '{}'\n"
	return writeExecutable(path, script)
}

// readFile reads a file for tests, failing the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
