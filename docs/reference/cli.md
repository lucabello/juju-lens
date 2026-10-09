# Command line

All `juju-lens` commands and their flags. For tasks, see the [how-to guides](../README.md#how-to-guides).

## `record`

```text
juju-lens record <name> [flags]
```

Records a controller into a new recording directory. `<name>` labels the recording.

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `./recordings/<ts>--<name>` | Recording directory. |
| `--attach` | `ssh` if `--ssh-target` is set, otherwise `local` | `local` or `ssh`. |
| `--controller` | current controller | Record every model on this controller. |
| `--model` | all models | Record only this model, written as `model` or `controller:model`. |
| `--ssh-target` | none | Machine to `juju ssh` into for `--attach ssh`, such as `controller/0`. |
| `--probe-path` | next to `juju-lens`, then `$PATH` | Path to the `juju-lens-probe` binary. |
| `--pid` | all agent processes | Record only these agent process IDs. Repeatable. |
| `--debug-log` | `true` | Stream `juju debug-log` for the recorded models. |
| `--k8s-log` | `true` | Stream workload-container logs for Kubernetes models. |
| `--machine-log` | `true` | Stream the systemd journal for machine models. |
| `--max-duration` | no limit | Stop recording after this duration, such as `30m` or `2h`. |
| `--max-size` | no limit | Stop recording after this many bytes of `raw/`. |
| `--detach` | `false` | Run the recorder in the background. Return once the probe has attached to at least one agent process. |
| `--detach-timeout` | `30s` | With `--detach`, fail if the probe hasn't attached within this time. Requires `--detach`. |

With `--detach`, the recorder writes its output to `recorder.log` in the recording directory. When it's ready, `record` exits 0 and prints two lines to standard output:

```text
dir=<absolute recording directory>
pid=<recorder process ID>
```

If the recorder exits or doesn't become ready within `--detach-timeout`, `record` stops it, removes `recorder.pid`, and exits 1 with the cause and the end of `recorder.log` on standard error. A probe that never reports its attach status is too old for `--detach`; the error says so, and upgrading `juju-lens-probe` on the target fixes it. See [how to record a controller in CI](../how-to/record-in-ci.md).

## `watch`

```text
juju-lens watch [model] [flags]
```

Records a model or controller on the local host and opens the viewer on the recording. Quitting the viewer stops the recording. Run with `sudo`.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | none | Watch only this model. Same as the `[model]` argument. |
| `--controller` | none | Watch every model on this controller. |
| `--output`, `-o` | `./recordings/<ts>--watch-<scope>` | Recording directory. |
| `--probe-path` | next to `juju-lens`, then `$PATH` | Path to the `juju-lens-probe` binary. |
| `--debug-log` | `true` | Stream `juju debug-log` for the recorded models. |

## `stop`

```text
juju-lens stop <recording> [flags]
```

Stops a running recorder, found through `recorder.pid`, with `SIGTERM`.

| Flag | Default | Meaning |
|---|---|---|
| `--timeout` | `30s` | How long to wait for the recorder to exit. |
| `--force` | `false` | Send `SIGKILL` if the recorder is still alive after `--timeout`. |

## `index`

```text
juju-lens index <recording> [flags]
```

Rebuilds `index.db` from a recording's `raw/` directory.

| Flag | Default | Meaning |
|---|---|---|
| `--rename` | none | Merge model `OLD` into model `NEW`, written as `OLD=NEW`. Repeatable. |

## `view`

```text
juju-lens view <recording> [flags]
```

Opens a recording in the viewer. See [viewer keybindings](viewer-keybindings.md).

| Flag | Default | Meaning |
|---|---|---|
| `--follow`, `-f` | `false` | Follow a recording that is still being written, and refresh as it grows. |

## `export`

```text
juju-lens export <recording> [flags]
```

Writes a recording's hooks, state changes, and logs as a folder of plain-text files, or as one document on standard output. See [how to export a recording for an AI agent](../how-to/export-for-an-agent.md) for the files.

| Flag | Default | Meaning |
|---|---|---|
| `--format` | none | Write a single merged document to stdout instead of a folder: `json` or `md`. |
| `--out` | `<recording>/derived/export` | Folder to write the export into. |
| `--parts` | all | Comma-separated files to write, from `summary`, `timeline`, `events`, `details`, and `report`. |
| `--model` | none | Export only this model. Required when the recording has more than one. |
| `--unit` | none | Export only this unit or application. Repeatable. |
| `--since` | none | Export only events at or after this time, in RFC 3339 format. |
| `--until` | none | Export only events at or before this time, in RFC 3339 format. |
| `--errors-only` | `false` | Only hooks that errored, were retried, or had an RPC error, with the hook before and after each on the same unit. |

## `synth`

```text
juju-lens synth <scenario> [flags]
```

Writes a recording that imitates a Juju controller, without needing one. The output is identical on every run for the same flags. The only scenario is
`trivial`: `grafana` and `prometheus` with one relation.

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `./recordings/synth-<scenario>` | Recording directory. |
| `--start` | fixed per scenario | Start time of the recording, in RFC 3339 format. |
| `--model` | `default` | Model name to use in the recording. |

## `version`

```text
juju-lens version
```

Prints the version and the commit it was built from.
