//go:build !linux

package recording

// HandToInvoker is Linux-only: recording is.
func HandToInvoker(string) error { return nil }

// OwnerHint is Linux-only: recording is.
func OwnerHint(string) string { return "" }
