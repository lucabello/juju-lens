# How to share a recording

Use this guide to pack a recording into one file, to attach to an issue, upload from a CI job, or hand to someone else, and to read it again on another machine.

## Prerequisites

- `juju-lens` installed where you pack and where you read. See [install](../../README.md#install). Reading a recording needs no probes, so a build from `go install` is enough.
- A recording that isn't being written. Stop a running recorder first; see [how to stop a running recording](stop-a-recording.md).

## Pack a recording

Pass the recording directory:

```bash
juju-lens export ./recordings/<timestamp>--my-capture
```

`export` writes `<timestamp>--my-capture.tar.gz` in the current directory and says how many files it packed. Use `--output` to choose the file:

```bash
juju-lens export ./recordings/my-capture --output my-capture.tar.gz
```

The file holds `manifest.json`, everything under `raw/`, and `recorder.log`. It leaves out `index.db`, which is as large as `raw/` and is rebuilt from it. A recording of five charms over nine minutes was 19 MB of raw data and packed to 2.1 MB.

## Read a packed recording

Extract the file, which makes a directory named after the recording, and ask it questions:

```bash
tar -xzf my-capture.tar.gz
juju-lens inspect summary my-capture
```

The first command that reads the recording builds the index, and says so on standard error. See [how to ask a recording questions](ask-a-recording-questions.md).

## Upload it from CI

Run `export` in a step that also runs when the tests fail, after `juju-lens stop`, and upload the file as the job artifact. [How to record a model in CI](record-in-ci.md) shows the steps.

For every flag, see the [command line reference](../reference/cli.md#export).
