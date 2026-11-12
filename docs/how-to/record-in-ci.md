# How to record a model in CI

Use this guide to record a Juju model in the background while a CI job or script runs tests against it. `juju-lens record --detach` starts the recorder, waits until it's capturing, and then returns, so the job fails at the start if the recording can't begin.

## Prerequisites

- Everything listed in [how to record a Juju controller](record-a-controller.md#prerequisites).
- The model and its units in place before the step starts. With the default `--attach unit`, the command waits until every unit in scope has a running probe; deploy the applications first, or they get a probe when they appear, after the command has returned.
- For `--attach host`, at least one Juju agent process running in the recording scope. Bootstrap the controller and add the model first.

## Start the recorder

Run `record` with `--detach` in its own step, before the tests. Set limits, so that a recording can't fill the runner's disk or outlive the job:

```bash
juju-lens record --detach ci-run --model testing -o ./recording \
    --max-duration 3h --max-size 2147483648
```

The command returns when every unit in scope has a running probe. From that point, what the units do is captured. On success it exits 0 and prints the recording directory and the recorder's process ID, one `key=value` pair per line:

```text
dir=/home/runner/work/my-charm/recording
pid=12345
```

If the recorder fails to start, the command exits non-zero and prints the cause along with the last lines of the recorder's log, so the step fails before any test runs. If the probes aren't running within 30 seconds, the command stops the recorder and fails. Use `--detach-timeout` to change the wait, for example on a model with many units:

```bash
juju-lens record --detach ci-run --model testing --detach-timeout 2m -o ./recording
```

To record the Juju API traffic as well, add `host`, and run the command with `sudo`. With `--attach host`, the command returns once the probe has attached to at least one agent process:

```bash
sudo juju-lens record --detach ci-run --model testing --attach host,unit -o ./recording
```

For a machine controller, `--attach host --ssh-target controller/0` runs the host probe there. If the error says the probe may be too old to support `--detach`, the probe never reported its attach status. To use `record --detach` with `--ssh-target`, upgrade `juju-lens-probe` on the controller machines. Older probes cause `--detach` to fail after `--detach-timeout`. Plain `record` is unaffected.

In all these cases no recorder is left running and no `recorder.pid` is left behind.

To read the output in a script:

```bash
out="$(juju-lens record --detach ci-run --model testing -o ./recording)"
dir="$(sed -n 's/^dir=//p' <<<"${out}")"
```

The recorder runs in its own session and ignores `SIGHUP`, so it keeps recording after the step's shell exits. It writes all of its output to `recorder.log` in the recording directory, so nothing it writes appears in later steps.

## Run the tests

Run the tests as usual. You don't need to wait or poll.

## Stop the recorder

After the tests, stop the recorder in a step that also runs when the tests fail:

```bash
juju-lens stop --timeout 120s ./recording
```

`stop` waits for the recorder to write the manifest, remove the probes from the units and build the index. On a model with many units, give it more than the default 30 seconds. `--force` kills a recorder that is still running when the timeout ends, which skips the probes' clean-up and leaves the index to rebuild with [`juju-lens index`](rebuild-the-index.md). For details, see [how to stop a running recording](stop-a-recording.md). To make sure the recorder stops even if the job is cancelled before this step, also pass `--max-duration` when you start it. When the recorder ran as root, as it does with `--attach host`, stop it with `sudo` too.

## Upload the recording

Pack the recording with `juju-lens export` and upload the file as a job artifact:

```bash
juju-lens export ./recording --output recording.tar.gz
```

It holds `recorder.log`, which you need to debug a recording that ended unexpectedly, and leaves out the index, which is as large as the raw data and is rebuilt where the recording is read. See [how to share a recording](share-a-recording.md) and [what a recording costs](../explanation/how-unit-capture-works.md#what-it-costs).

For every flag, see the [command line reference](../reference/cli.md#record).
