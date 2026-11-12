# How to share a recording

Use this guide to pack a recording, or part of it, into one file, to attach to an issue, upload from a CI job, or hand to someone else, and to read it again on another machine.

## Prerequisites

- `juju-lens` installed where you pack and where you read. See [install](../../README.md#install). Reading a recording needs no probes, so a build from `go install` is enough.
- A recording that isn't being written. Stop a running recorder first; see [how to stop a running recording](stop-a-recording.md).

## Pack a recording

Pass the recording directory:

```bash
juju-lens export ./recordings/<timestamp>--my-capture
```

Replace `<timestamp>` with the one in the directory's name.

`export` writes `<timestamp>--my-capture.tar.gz` in the current directory and says how many files it packed. Use `--output` to choose the file:

```bash
juju-lens export ./recordings/my-capture --output my-capture.tar.gz
```

The file holds `manifest.json`, everything under `raw/`, and `recorder.log`. It leaves out `index.db`, which is as large as `raw/` and is rebuilt from it. For example, a nine-minute recording of five charms made with `--attach unit` was 19 MB of raw data and packed to 2.1 MB.

## Pack part of a recording

To share only what matters, scope the export to a model, a unit or application, or a time window:

```bash
juju-lens export ./recordings/my-capture --unit grafana --since 2026-11-11T17:55:00Z --until 2026-11-11T17:58:00Z
```

The archive is a recording of its own, and its manifest says that it is a part: `inspect summary` shows the scope. A hook that straddles the window is cut: `inspect event` marks it as truncated, with the reason, and a hook that began before `--since` is listed. A hook wholly outside the window is not. If you packed one model of several, `inspect` needs no `--model` for it.

Scoping chooses what to include. It does not redact. The status snapshots of a model (`bootstrap.json`, `databags.json`, `config.json`) are kept whole and `databags.json` and `config.json` hold the values of relation data and configuration; pass `--without-values` to leave those two out. Logs hold whatever the charms wrote to them. Review the files before you post the archive publicly. The [command line reference](../reference/cli.md#export) describes what each flag keeps.

## Read a packed recording

Extract the file, which makes a directory named after the recording, and ask it questions:

```bash
tar -xzf my-capture.tar.gz
juju-lens inspect summary my-capture
```

The first `inspect` command builds the index, and says so on standard error. To open the recording in the viewer, `juju-lens view` does the same. See [how to ask a recording questions](ask-a-recording-questions.md).

## Upload it from CI

Run `export` in a step that also runs when the tests fail, after `juju-lens stop`, and upload the file as the job artifact. [How to record a model in CI](record-in-ci.md) shows the steps.

For every flag, see the [command line reference](../reference/cli.md#export).
