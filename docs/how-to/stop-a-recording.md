# How to stop a running recording

Use this guide to stop a `juju-lens record` or `juju-lens watch` process that you can't reach with `Ctrl-C`, such as one running in the background or under systemd.

## Stop the recorder

Pass the recording directory:

```bash
juju-lens stop ./recordings/my-capture
```

`stop` reads the recorder's process ID from `recorder.pid` in that directory and sends it `SIGTERM`, the same signal as `Ctrl-C`. The recorder detaches from the agents, writes the manifest, finishes building the index, and removes `recorder.pid`. `stop` waits up to 30 seconds for it to exit. Use `--timeout` to change that.

If there's no `recorder.pid`, the recorder isn't running.

## Force the recorder to stop

If the recorder doesn't exit in time, send it `SIGKILL` instead:

```bash
juju-lens stop --force --timeout 5s ./recordings/my-capture
```

A killed recorder doesn't finish its shutdown, so the manifest is incomplete and the index may be out of date. [Rebuild the index](rebuild-the-index.md) before opening the recording.

For every flag, see the [command line reference](../reference/cli.md#stop).
