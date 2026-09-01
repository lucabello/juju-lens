# CLI reference

Every `juju-lens` subcommand and its flags. For task-oriented walkthroughs,
see the [how-to guides](../README.md#how-to-guides).

## `record`

```
juju-lens record <name> [flags]
```

Attaches the eBPF probe (locally or over SSH) and captures RPCs and logs
into a new recording directory. `<name>` labels the recording; it is not
passed to `juju`.

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `./recordings/<ts>--<name>` | Recording directory. |
| `--attach` | auto-detect | `local` or `ssh`. |
| `--controller` | current controller | Scope to this Juju controller (all its models). |
| `--model` | all models | Scope to a single model, as `model` or `controller:model`. |
| `--ssh-target` | — | Machine to `juju ssh` into for `--attach ssh` (e.g. `controller/0`). |
| `--probe-path` | next to `juju-lens`, then `$PATH` | Path to the `juju-lens-probe` binary. |
| `--pid` | all `jujud`/`containeragent` PIDs | Restrict the probe to these PIDs (repeatable). |
| `--debug-log` | `true` | Stream `juju debug-log` for the scoped models. |
| `--k8s-log` | `true` | Stream workload-container logs for CAAS models. |
| `--machine-log` | `true` | Stream machine journald for IAAS models. |
| `--max-duration` | no limit | Stop recording after this duration (e.g. `30m`, `2h`). |
| `--max-size` | no limit | Stop recording after this many bytes of `raw/`. |

## `watch`

```
juju-lens watch [model] [flags]
```

Records a live model or controller and immediately opens the viewer
following it. Requires `sudo` (eBPF needs root). Quitting the viewer (`q`)
stops the recording.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | — | Scope to a single model (also accepted as the positional argument). |
| `--controller` | — | Scope to a controller (all its models). |
| `--output`, `-o` | `./recordings/<ts>--watch-<scope>` | Recording directory. |
| `--probe-path` | — | Path to the `juju-lens-probe` binary. |
| `--debug-log` | `true` | Stream `juju debug-log` for the scoped models. |

## `stop`

```
juju-lens stop <recording> [flags]
```

Signals a running recorder (found via `recorder.pid`) to shut down cleanly.

| Flag | Default | Meaning |
|---|---|---|
| `--timeout` | `30s` | Wait this long for the recorder to exit before giving up. |
| `--force` | `false` | Send `SIGKILL` if the recorder is still alive after `--timeout`. |

## `index`

```
juju-lens index <recording> [flags]
```

Rebuilds `index.db` from a recording's `raw/` tree. Safe to run repeatedly.

| Flag | Default | Meaning |
|---|---|---|
| `--rename` | — | Remap a model during rebuild, as `OLD=NEW` (repeatable); merges `OLD`'s spans, snapshots, and logs into `NEW`. |

## `view`

```
juju-lens view <recording> [flags]
```

Opens a recording in the TUI viewer.

| Flag | Default | Meaning |
|---|---|---|
| `--follow`, `-f` | `false` | Tail a live (still-growing) recording, refreshing as it grows. |

## `export`

```
juju-lens export <recording> [flags]
```

Writes a recording's derived narrative as a folder of plain-text artifacts,
or a single document to stdout. See [how to export a recording for an
agent](../how-to/export-for-an-agent.md) for the file list.

| Flag | Default | Meaning |
|---|---|---|
| `--format` | — | Write a single merged document to stdout instead of a folder: `json` or `md`. |
| `--out` | `<recording>/derived/export` | Folder to write the export into. |
| `--parts` | all | Comma-separated subset to write: `summary,timeline,events,details,report`. |
| `--model` | — | Focus on a single model (required if the recording has more than one). |
| `--unit` | — | Restrict to this unit or application (repeatable). |
| `--since` | — | RFC3339 lower time bound. |
| `--until` | — | RFC3339 upper time bound. |
| `--errors-only` | `false` | Only hook runs that ended errored/retried/rpc-warn, plus one same-unit neighbour on each side. |

## `synth`

```
juju-lens synth <scenario> [flags]
```

Writes a recording directory whose spans mimic a real Juju controller,
without needing one. Reproducible byte-for-byte when `--start` is fixed.
Available scenarios: `trivial` (two applications, `grafana` and
`prometheus`, forming one relation).

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `./recordings/synth-<scenario>` | Recording directory. |
| `--start` | scenario's deterministic default | RFC3339 start instant. |
| `--model` | `default` | `juju.model` resource attribute stamped on every span. |

## `version`

```
juju-lens version
```

Prints the build stamp (version and commit).
