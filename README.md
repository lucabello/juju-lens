# juju-lens

`juju-lens` records what a Juju controller and its models do, and lets you browse the recording offline. It shows the hooks each unit ran, the status and relation data they changed, and the logs written at the time, all on one timeline.

It reads Juju API traffic from inside the Juju agents with read-only eBPF probes, so it works with any charm and doesn't change anything on the controller. It supports Juju 3.6 and 4.x, on machine and Kubernetes controllers.

## Install

Install the latest release of `juju-lens` and `juju-lens-probe`:

```bash
curl -fsSL https://raw.githubusercontent.com/lucabello/juju-lens/main/install.sh | sh
```

The script installs to `/usr/local/bin`, or `~/.local/bin` if that isn't writable. Set `JUJU_LENS_VERSION` to install a specific release, or `JUJU_LENS_INSTALL_DIR` to choose the directory. Releases are published for Linux on `amd64` and `arm64`.

To build from source instead, you need Go 1.25 or newer:

```bash
# install both binaries at once
go install github.com/lucabello/juju-lens/cmd/...@latest

# or, equivalently, one at a time
go install github.com/lucabello/juju-lens/cmd/juju-lens@latest
go install github.com/lucabello/juju-lens/cmd/juju-lens-probe@latest
```

Either way, the binaries go into `$GOBIN` (by default `~/go/bin`).

Recording needs Linux 5.8 or newer, and root or `CAP_BPF`, on the host where the Juju agents run. Viewing a recording works anywhere `juju-lens` builds.

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

[Your first recording](docs/tutorials/first-recording.md) walks through a full example. The [documentation](docs/README.md) covers the rest, including how capture works.

## Limitations

- Kubernetes controllers can only be recorded from the host they run on, such as a local MicroK8s. Recording a remote Kubernetes cluster isn't supported yet.
- Machine controllers can be recorded remotely over `juju ssh`, but `juju-lens-probe` must already be installed on the controller machine.

## Develop

The repository uses [`just`](https://github.com/casey/just) for common tasks:

```bash
just build   # build ./bin/juju-lens and ./bin/juju-lens-probe
just demo    # generate a synthetic recording and open it
just check   # format, lint, and test
```

Run `just` to list every recipe. To publish a release, push a `vX.Y.Z` tag; GitHub Actions builds the binaries and attaches them to a GitHub release.

## License

Apache-2.0.
