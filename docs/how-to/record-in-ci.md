# How to record a controller in CI

Use this guide to record a Juju controller in the background while a CI job or script runs tests against it. `juju-lens record --detach` starts the recorder, waits until it's capturing, and then returns, so the job fails at the start if the recording can't begin.

## Prerequisites

- Everything listed in [how to record a Juju controller](record-a-controller.md#prerequisites).
- At least one Juju agent process running in the recording scope before the step starts. Bootstrap the controller and add the model first.

## Start the recorder

Run `record` with `--detach` in its own step, before the tests:

```bash
sudo juju-lens record --detach ci-run --model testing -o ./recording
```

The command returns when the probe has attached to at least one agent process. From that point, every RPC is captured. On success it exits 0 and prints the recording directory and the recorder's process ID, one `key=value` pair per line:

```text
dir=/home/runner/work/my-charm/recording
pid=12345
```

If the recorder fails to start, the command exits non-zero and prints the cause along with the last lines of the recorder's log, so the step fails before any test runs. If the probe hasn't attached to an agent within 30 seconds, the command stops the recorder and fails. Use `--detach-timeout` to change the wait, for example on a slow `--attach ssh` connection:

```bash
juju-lens record --detach ci-run --ssh-target controller/0 --detach-timeout 2m -o ./recording
```

In both cases no recorder is left running and no `recorder.pid` is left behind.

To read the output in a script:

```bash
out="$(sudo juju-lens record --detach ci-run --model testing -o ./recording)"
dir="$(sed -n 's/^dir=//p' <<<"${out}")"
```

The recorder runs in its own session and ignores `SIGHUP`, so it keeps recording after the step's shell exits. It writes all of its output to `recorder.log` in the recording directory, so nothing it writes appears in later steps.

## Run the tests

Run the tests as usual. You don't need to wait or poll.

## Stop the recorder

After the tests, stop the recorder in a step that also runs when the tests fail:

```bash
sudo juju-lens stop ./recording
```

`stop` waits for the recorder to write the manifest and build the index. For details, see [how to stop a running recording](stop-a-recording.md). To make sure the recorder stops even if the job is cancelled before this step, also pass `--max-duration` when you start it.

## Upload the recording

Upload the whole recording directory as a job artifact. It includes `recorder.log`, which you need to debug a recording that ended unexpectedly.

For every flag, see the [command line reference](../reference/cli.md#record).
