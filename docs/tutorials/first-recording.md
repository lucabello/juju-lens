# Your first recording

In this tutorial you'll record a Juju controller on MicroK8s while the integration tests of a real charm deploy and relate applications on it. Then you'll open the recording and find the hooks those tests triggered.

It takes about 30 minutes, most of it waiting for the tests. If you only want to try the viewer, [generate a synthetic recording](../how-to/generate-a-synthetic-recording.md) instead.

## Prerequisites

- A Linux machine with kernel 5.8 or newer, where you have root.
- [MicroK8s](https://microk8s.io/) with a Juju controller bootstrapped on it, and `charmcraft` and `tox` installed. The [Juju documentation](https://documentation.ubuntu.com/juju/3.6/howto/manage-charms/) describes this setup.
- `juju-lens` built from a clone of this repository with `just build`.
- `git`.

MicroK8s runs pods as ordinary processes on the host, so `juju-lens` can find the controller's agent there. Recording a Kubernetes controller on another machine isn't supported yet.

## Get a charm

This tutorial uses [`prometheus-k8s-operator`](https://github.com/canonical/prometheus-k8s-operator). Its tests deploy the charm, relate it to a tester charm, change configuration, and refresh, which together produce a varied set of hooks.

```bash
git clone https://github.com/canonical/prometheus-k8s-operator
cd prometheus-k8s-operator
```

Any charm with integration tests works. If you maintain one, you can use it instead.

## Start recording

Check that the MicroK8s controller is your current controller:

```bash
juju controllers
```

The current controller is marked with `*`. In the directory where you built `juju-lens`, start a recording called `int-test`:

```bash
sudo ./bin/juju-lens record int-test --attach local
```

Without `--controller` or `--model`, `juju-lens` records every model on the current controller, including models created after it starts. The tests will create one. Leave this command running.

## Run the tests

In a second terminal, in the `prometheus-k8s-operator` directory, run one test module:

```bash
tox -e integration -- -k test_charm
```

This takes several minutes. If `test_charm` no longer exists, pick another short module from `tests/integration/`. The tests don't need to pass: a failing test still runs hooks, and failures are worth looking at too.

## Stop recording

When the tests finish, go back to the first terminal and press `Ctrl-C`. The recorder detaches from the agents, finishes building the index, and prints where it saved the recording:

```text
juju-lens: recording saved to recordings/2026-10-06T10-12-03--int-test (5120 RPC messages, 8803 log lines captured)
```

Your directory name and counts will differ.

## Explore the recording

Open the recording in the viewer, using the directory from the previous step:

```bash
./bin/juju-lens view recordings/2026-10-06T10-12-03--int-test
```

The recording has more than one model, so the viewer asks which one to show first. Choose the model the tests created; its name starts with `test-`. You can switch models later with `m`.

The viewer has three panes. **Events** is on the top left and lists the hooks each unit ran. **Status** is on the top right and shows each application and unit as of the selected event. **Logs** runs along the bottom.

Press `↓` to move through the Events pane. Near the top you'll see each unit run `install`, `config-changed`, and `start`. As you move, the Status pane changes to match: a unit that was `waiting` becomes `active` at the event where its charm set that status.

Further down, find a hook whose name ends in `-relation-joined`. This is the tester charm relating to Prometheus. Press `Enter` to open the inspector, which shows the relation data the hook wrote and the RPCs it made. Press `Enter` again to close it.

Press `.` to switch to verbose mode. Every status call, relation scope change, and leadership claim now has its own row. Press `.` again to go back.

Finally, find an `upgrade-charm` hook. This is the refresh at the end of the tests.

Press `q` to quit.

You recorded a controller without changing its configuration or the charms on it. To record your own deployments, see [how to record a controller](../how-to/record-a-controller.md). To understand what the Events pane shows and how it's built, read the [explanation pages](../README.md#explanation) in order, starting with [how capture works](../explanation/how-capture-works.md).
