# juju-lens

`juju-lens` records what a Juju controller and its models do, and lets you browse the recording offline. It shows the hooks each unit ran, the status and relation data they changed, and the logs written at the time, all on one timeline.

It can capture two sides of a model. From the host, it reads Juju API traffic inside the Juju agents with read-only eBPF probes, so it works with any charm and doesn't change anything on the controller. From inside each unit, over `juju ssh`, it records what a charm built on [ops](https://github.com/canonical/operator) does in each hook: its events, observers, hook tool and Pebble calls, what the hook cost in CPU and memory, and where its Python spent that time. It supports Juju 3.6 and 4.x, on machine and Kubernetes controllers.

## Install

Install the latest release of `juju-lens`, a single binary:

```bash
curl -fsSL https://raw.githubusercontent.com/lucabello/juju-lens/main/install.sh | sh
```

The script installs to `/usr/local/bin`, or `~/.local/bin` if that isn't writable. Set `JUJU_LENS_VERSION` to install a specific release, or `JUJU_LENS_INSTALL_DIR` to choose the directory. Releases are published for Linux on `amd64` and `arm64`.

`juju-lens` carries the probe inside itself: a static build for `amd64` and one for `arm64`. When it records with `--attach unit`, it uploads the one that matches each unit, so the units' architecture doesn't have to be yours, and nothing else needs installing.

To build from source, you need Go 1.25 or newer and [`just`](https://github.com/casey/just):

```bash
just build   # builds ./bin/juju-lens, with both probes embedded
```

`go install github.com/lucabello/juju-lens/cmd/juju-lens@latest` builds `juju-lens` without the embedded probes, because they are built first. That build can view and export recordings, record with `--attach none`, and record the host it runs on with `--attach host`; it can't upload a probe, so `--attach unit` and `--ssh-target` need a release or `just build`.

What recording needs depends on what you capture, chosen with `record --attach`:

| `--attach` | Captures | Needs |
|---|---|---|
| `host` (default) | The agents' Juju API calls. | Linux 5.8 or newer, and root or `CAP_BPF`, on the host where the Juju agents run. |
| `unit` | Inside each charm: events, observers, hook tool and Pebble calls, each hook's CPU and memory, and Python stack samples. Writes a module into each charm's venv while it runs, and removes it afterwards. | A Juju client that can `juju ssh` into the units; on machine models, passwordless `sudo` on the machines. Nothing installed on the units beforehand: `juju-lens` uploads its probe, for the units' architecture (`amd64` or `arm64`). Workload-container logs on Kubernetes come through Pebble, so no `kubectl` is needed. |
| `host,unit` | Both, joined hook by hook. | Both of the above. |
| `none` | Logs and the model status only. | The Juju client. |

Viewing a recording works anywhere `juju-lens` builds.

## Use

Record the current controller until you press `Ctrl-C`:

```bash
sudo juju-lens record my-capture
```

Then open the recording:

```bash
juju-lens view ./recordings/<timestamp>--my-capture
```

To record and view at the same time, use `sudo juju-lens watch <model>`.

To see inside the charms instead, with nothing but `juju ssh` (and `sudo` on machine models), record a model with `--attach unit`. Every unit of the model gets a probe:

```bash
juju-lens record my-capture --model <model> --attach unit
```

[How to trace a charm's memory spike to its code](docs/how-to/trace-a-memory-spike.md) shows what to do with such a recording, and [how unit capture works](docs/explanation/how-unit-capture-works.md) describes what it changes on the units.

[Your first recording](docs/tutorials/first-recording.md) walks through a full example. The [documentation](docs/README.md) covers the rest, including how capture works.

## Limitations

- With `--attach host`, Kubernetes controllers can only be recorded from the host they run on, such as a local MicroK8s, and machine controllers remotely over `juju ssh`, to which `juju-lens` uploads the probe, with `sudo` on the controller machine. To record the charms of a model on a remote Kubernetes cluster, use `--attach unit`, which needs only `juju ssh` but doesn't capture Juju API traffic.
- `--attach unit` records events, observers and stacks only for charms built on ops; other charms, and `juju exec`, get each execution's duration and cost only. It doesn't see Juju's side, such as the agent's `run-hook` markers, its status, and relation data the charm doesn't read; add `host` for that. On Kubernetes, the first hooks after a refresh run before the module is back in the new pod.

## Develop

The repository uses [`just`](https://github.com/casey/just) for common tasks:

```bash
just build   # build ./bin/juju-lens, with the probes inside it
just demo    # generate a synthetic recording and open it
just check   # format, lint, and test
```

Run `just` to list every recipe. To publish a release, run `just release vX.Y.Z` from an up-to-date `main`. It checks the version is newer than the latest tag, runs `just check`, then tags and pushes; GitHub Actions builds the binaries (`just dist`) and attaches them to a GitHub release.

`--attach unit` installs a Python module, `internal/unit/pytap/_juju_lens_tap.py`, that patches internals of [ops](https://github.com/canonical/operator), not a public API. `just ops-matrix` checks it against a range of ops releases: for each row of `MATRIX` in `internal/unit/pytap/opsmatrix/matrix.py` it builds a venv with `uv` (cached in `~/.cache/juju-lens/opsmatrix`), runs a fake charm through a sequence of hooks and an action with and without the module, and checks what the module recorded, that no secret value reached a record, and that the charm's exit status, output, and ops tracing spans didn't change. It needs `uv` and network access, so it isn't part of `just check`. When ops makes a release, add it to `MATRIX` (with `True` in the last field if the release has `ops-tracing`, and the oldest Python it supports) and run `just ops-matrix --only <version>`. If a check fails, `-v` prints every check with what it found.

## License

Apache-2.0.
