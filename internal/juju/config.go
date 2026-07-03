// Package juju wraps the small piece of the juju CLI that juju-lens needs.
// M5 will replace this with the juju/juju/api client library; for now the
// CLI shell-out is simpler, matches what users already have on their
// PATH, and works for both machine and k8s controllers.
package juju

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// OTEL config keys we manage. Order matters: 'enabled' goes last on set so
// the endpoint and other params are in place before Juju starts trying to
// export.
var OTELKeys = []string{
	"open-telemetry-endpoint",
	"open-telemetry-insecure",
	"open-telemetry-sample-ratio",
	"open-telemetry-stack-traces",
	"open-telemetry-tail-sampling-threshold",
	"open-telemetry-enabled",
}

// Client shells out to `juju`. Controller (optional) is passed via -c so
// callers can target a non-current controller.
type Client struct {
	Bin        string // defaults to "juju" via PATH
	Controller string // "" means "current"
}

// New returns a Client using the "juju" binary on PATH.
func New(controller string) *Client {
	return &Client{Bin: "juju", Controller: controller}
}

// Available reports whether the juju CLI is on PATH and responds to
// --version. Callers use this to skip auto-configure gracefully.
func (c *Client) Available() error {
	bin := c.Bin
	if bin == "" {
		bin = "juju"
	}
	cmd := exec.Command(bin, "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("juju CLI not usable (looked for %q on PATH): %w", bin, err)
	}
	return nil
}

// ReadOTELConfig returns the current values for every OTEL key. Missing
// keys map to the empty string; the caller uses this to know what to
// restore later.
func (c *Client) ReadOTELConfig() (map[string]string, error) {
	// One shot: `juju controller-config --format json` prints every key
	// currently set, so a single fetch covers all six. When Controller is
	// empty the juju CLI targets the currently active controller.
	args := []string{"controller-config", "--format", "json"}
	if c.Controller != "" {
		args = append(args, "--controller", c.Controller)
	}
	out, err := c.run(args...)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing controller-config JSON: %w", err)
	}
	values := map[string]string{}
	for _, k := range OTELKeys {
		v, ok := raw[k]
		if !ok {
			values[k] = ""
			continue
		}
		values[k] = stringify(v)
	}
	return values, nil
}

// SetOTELConfig applies key=value pairs via `juju controller-config`. Only
// the pairs whose value is non-empty are sent, so callers can pass a
// partial map when restoring (empty means "leave alone at Juju's default"
// on the initial set, or "reset" on restore — see UnsetOTELConfig).
func (c *Client) SetOTELConfig(kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	args := []string{"controller-config"}
	if c.Controller != "" {
		args = append(args, "--controller", c.Controller)
	}
	// Send keys in the OTELKeys order so 'enabled' lands last.
	set := 0
	for _, k := range OTELKeys {
		v, ok := kv[k]
		if !ok || v == "" {
			continue
		}
		args = append(args, fmt.Sprintf("%s=%s", k, v))
		set++
	}
	if set == 0 {
		return nil
	}
	_, err := c.run(args...)
	return err
}

// UnsetOTELConfig resets keys to Juju defaults with `juju controller-config
// --reset key1,key2,...`. Used to fully undo auto-configuration when no
// prior value was recorded.
func (c *Client) UnsetOTELConfig(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	args := []string{"controller-config"}
	if c.Controller != "" {
		args = append(args, "--controller", c.Controller)
	}
	args = append(args, "--reset", strings.Join(keys, ","))
	_, err := c.run(args...)
	return err
}

// RestoreOTELConfig puts the controller back the way we found it. Keys
// that had a value get set to that value; keys that were unset get reset.
func (c *Client) RestoreOTELConfig(previous map[string]string) error {
	var toReset []string
	toSet := map[string]string{}
	for _, k := range OTELKeys {
		v := previous[k]
		if v == "" {
			toReset = append(toReset, k)
			continue
		}
		toSet[k] = v
	}
	var errs []error
	if err := c.SetOTELConfig(toSet); err != nil {
		errs = append(errs, fmt.Errorf("setting previous values: %w", err))
	}
	if err := c.UnsetOTELConfig(toReset); err != nil {
		errs = append(errs, fmt.Errorf("resetting keys: %w", err))
	}
	return errors.Join(errs...)
}

func (c *Client) run(args ...string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "juju"
	}
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w (stderr: %s)",
			bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// stringify normalises whatever JSON type Juju returned into the string
// representation the CLI accepts on set. Bool→"true"/"false", numbers→their
// canonical decimal form; strings pass through.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// Prefer integer form when possible so "sample-ratio=1" round-trips.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
