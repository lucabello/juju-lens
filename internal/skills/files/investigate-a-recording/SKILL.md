---
name: investigate-a-recording
description: Investigate a juju-lens recording of a Juju model: find the hooks that failed or cost the most CPU, memory or time, and the charm code, logs and memory behind them. Use when given a recording directory or a packed recording (.tar.gz) from juju-lens, or asked why a charm is slow, uses memory, or a hook failed.
---

# Investigate a juju-lens recording

A recording is a directory made by `juju-lens record`. `juju-lens inspect` answers questions about it as short text. Run it, rather than reading files under the recording. A packed recording (`.tar.gz`, from `juju-lens export`) is extracted first with `tar -xzf`; the first `inspect` command on a recording builds its index and says so.

Every list is bounded, says how much it left out (`--skip`, `--limit`), and ends with `next:` lines naming the command to run next. Every command takes `--json`. `juju-lens inspect --help` lists the commands.

## Path

1. `juju-lens inspect summary <recording>`: when it was taken, how many events each unit ran and how they ended, the slowest events, the hooks with the most CPU. A recording with several models needs `--model`.
2. Pick the hooks: `juju-lens inspect events <recording> --fail problem` for hooks that errored or retried; `--sort cpu`, `--sort mem` or `--sort duration` to rank; `--unit`, `--name config-changed`, `--since`, `--until` to narrow. Each line ends with a span ID.
3. Sum over hooks: `juju-lens inspect costs <recording>` adds up charm init and observers by observer (the default), `--by event`, `--by unit` or `--by app`, sorted by `--sort cpu`, `mem` or `duration`.
4. Go into one hook with its span ID:
   - `juju-lens inspect event <recording> <span-id>`: why it failed, what it did, its heaviest stacks.
   - `juju-lens inspect trace <recording> <span-id>`: what ran when, with the charm's functions under each span.
   - `juju-lens inspect memory <recording> <span-id>`: how memory moved around the hook, the biggest jumps and the observer and code running at each.
   - `juju-lens inspect profile <recording> --span <span-id>`: the functions the time went to; views `top` (default), `peek <function>`, `tree` and `stacks`; `--metric wall|cpu|mem`; `--detail charm` for the charm's own code only; `--order total` to rank by what a function called too.
   - `juju-lens inspect logs <recording> --span <span-id>`: the log lines about the hook; filter logs by `--unit`, `--level`, `--grep`, `--since`, `--until`.
5. Compare: `juju-lens inspect diff <recording> --span-a <id> --span-b <id>` for two hooks, or `juju-lens inspect diff <before> <after> --unit <u> --name <event>` for the same hook in two recordings.
6. For a flame graph tool or `grep`: `juju-lens inspect profile <recording> --unit <u> stacks --folded`.

## What the numbers mean

- CPU and memory of a hook count charm init and the observers, with what they called. The time Python spends importing modules before ops starts is not in them; the profile shows it as `(importing Python modules)`.
- A hook's CPU includes the processes the hook started that were sampled too. Ranking by the main process alone can give another hook first.
- Memory taken is the growth of resident memory between two samples (10 ms apart), summed: it says where memory was taken, not where it was held. A function that frees later still counts.
- Wall time includes waiting, for example on Pebble or hook tools; CPU does not. A function with much wall time and little CPU was waiting.
- Sizes use 1024-based units labelled `KB`, `MB`.
- Costs, profiles, traces and memory exist only for hooks recorded with `--attach unit`; a command that needs stack samples says so when the recording has none. Events and logs do not need them.
- Span IDs come from `events`, `costs` (the costliest span of a group) and `profile stacks`. An ID of a span inside a hook (an observer, charm init) works wherever a span ID is taken, and `event` says which hook it is in.

## Sharing

`juju-lens export <recording>` packs a recording into one `.tar.gz`; `--unit`, `--model`, `--since` and `--until` pack part of it.
