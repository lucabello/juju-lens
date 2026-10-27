# Viewer keybindings

Keys for `juju-lens view` and `juju-lens watch`.

## Main view

| Key | Pane | Action |
|---|---|---|
| `↑`/`k`, `↓`/`j` | Any | Move the cursor. |
| `PgUp`/`b`, `PgDn` | Any | Page up or down. |
| `g`/`Home`, `G`/`End` | Any | Jump to the first or last row. |
| `←`/`h`, `→`/`l` | Any | Scroll a long line horizontally. |
| `w` | Any | Toggle line wrapping. |
| `Tab`, `Shift+Tab` | Any | Move focus to the next or previous pane. |
| `Enter` | Events | Open the inspector for the selected event. A hook recorded with `--attach unit` opens on its [trace](#trace); other events open on what they set or changed: statuses, relation data changes, and RPCs. |
| `d` | Events | Open the databag and config changes of the selected event, as a diff, for an event that changed a relation databag or the charm config. |
| `.` | Events | Toggle verbose mode, which shows individual RPCs and hook outcomes hidden by default. See [what the viewer shows](../explanation/rpcs-to-hooks.md#what-the-viewer-shows). |
| `n`, `N` | Events | Jump to the next or previous event on the same unit. |
| `Space` | Status | Show only the selected application or unit in all panes, or remove that filter. Selecting an application includes its units. |
| `f` | Logs | Toggle between following the selected event and scrolling freely. |
| `/` | Events, Logs | Filter the focused pane by text. In Events, a hook recorded with `--attach unit` also matches the names of the events and observers the charm ran in it. |
| `r` | Any | Clear the `Space` and `/` filters. |
| `m` | Any | Choose a model, in recordings with more than one. |
| `q`, `Esc`, `Ctrl+C` | Any | Quit. |

## Inspector and model picker

| Key | Window | Action |
|---|---|---|
| `d` | Inspector | On the databag and config changes of an event, toggle showing them as a diff. From the trace, the profile or the memory view, show those changes as a diff. |
| `y`, `Y` | Inspector | Copy the value after or before the change. |
| `t` | Inspector | Show the trace of a hook recorded with `--attach unit`: what ran when, along a time axis. This is where such a hook opens. |
| `p` | Inspector | Show the profile of a hook recorded with `--attach unit`: a call tree of the charm's functions, or a table. |
| `m` | Inspector | Show what happened to memory around a hook recorded with `--attach unit`. |
| `Esc`, `q` | Inspector | From the profile, the memory view or the changes, go back to the trace; from the trace, or for an event without one, close the inspector. |
| `Enter` | Inspector | Close the inspector, for an event without a trace. |
| `↑`/`k`, `↓`/`j` | Model picker | Move the cursor. |
| `g`/`Home`, `G`/`End` | Model picker | Jump to the first or last model. |
| `Enter`, `Space` | Model picker | Choose the selected model. |
| `q`, `Esc`, `Ctrl+C` | Model picker | Close without changing model. |

## Trace

`t` in the inspector draws a hook recorded with `--attach unit` along time, like a distributed trace, and it is where such a hook opens. Each row is a bar on a common time axis, indented under the row it ran in: the process, the charm's `__init__`, events and observers, the functions of the charm's own code that ran in them, and the hook tool and Pebble calls that took long. The functions are rebuilt from the stack samples, so a function shorter than a few samples isn't drawn, and a bar's ends are accurate to about 10 ms. A call sits under the function that was running when it began, so a Pebble wait shows under the function that made it. Above the rows, one line draws the hook process's resident memory over the same axis, from its lowest value to its highest, so a rise lines up with the rows that ran then.

By default the trace shows the charm's code and the calls it makes: the ops and library functions each charm function calls directly, such as `Container.replan` or `Container.exec`. They explain the time between a charm function and the Pebble call it caused, which sits under them. It hides what is only noise: deeper ops internals, the top level of modules being imported, short calls and short functions; the time spent importing Python modules before ops started is said in the header. `.` shows everything, and again goes back.

The first row under the process is the charm's `__init__`: ops builds the charm by calling it, and the time ops takes around it is small.

Bars are blue for events and observers, orange for charm functions, green for ops functions, yellow for Pebble calls, grey for hook tools, and red for a span that failed. On the right of each row are the time it took and the memory it took, when that is 1 MB or more. The header says how many levels are open. Under the rows, the selected row says when it started, how long it ran and its memory; for a function it says `mostly waiting` when it used little CPU for the time it took, and for what: a Pebble call, a hook tool, a subprocess, the network, or the function it was blocked in.

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Select a row. |
| `←`/`h` | Fold the selected row's children, or select the row it is under. |
| `→`/`l` | Unfold. |
| `Enter`, `Space` | Fold or unfold the selected row. |
| `1`–`9` | Open that many levels of the whole trace: `1` the process alone, `2` its direct children, and so on. Deeper rows are folded. |
| `+`, `-` | Open one more level, or close one, under the selected row only, so you can open one branch deeper while the rest stays as it is. `-` folds the deepest open level under the row, or, on a row with nothing open under it, folds the row it is under and selects that; `+` unfolds the shallowest folded level. |
| `.` | Show everything: all code however short or deep, including ops internals, imports and short calls, and the CPU time in the details of a row. Again, back to the default. |
| `Tab` | For a hook Juju ran more than once, the next attempt. |
| `Esc`, `q` | Close the inspector. |
| `p`, `m`, `d` | Show the profile, the memory view, or the databag and config changes instead. |

## Profile

`p` in the inspector opens the profile of a hook recorded with `--attach unit`: how much of the hook's time, CPU time or memory each function of the charm took, from the stack samples. The trace (`t`) is the better view for finding what took long; the profile adds up what the same function took over the whole hook, and shows the callers and callees of each.

It opens as a call tree. Function names are orange for charm code, green for ops, and plain for everything else.

| Key | Action |
|---|---|
| `Tab`, `Shift+Tab` | Look at the next or previous part of the hook: the whole hook, the charm's `__init__`, or one observer. |
| `s` | Weigh the functions by time, CPU time, or memory taken. |
| `v` | Switch between the call tree and a table of functions. |
| `.` | Show all code, including ops, the standard library, imports and wrappers, and again to go back to the charm's code and the calls it makes. |
| `/` | Search function names and files, as a case-insensitive regular expression. `Enter` selects the first match, and the views mark every match. |
| `n`, `N` | Select the next or previous match. |
| `↑`/`k`, `↓`/`j` | Select a function. |
| `Enter` | In the tree, start the tree from the selected function. In the table, show what called the selected function and what it called. |
| `Backspace` | Go back one step from `Enter`. |
| `Esc` | Undo the narrowest thing in effect: the search being typed, the callers and callees, the tree's starting point, then the search. With none, go back to the trace. |
| `t`, `m`, `d` | Show the trace, the memory view, or the databag and config changes instead. |
| `q` | Go back to the trace. |

### Code shown

By default the profile shows the charm's own code and the calls it makes: the ops and library function each charm function calls directly. What the code it hides cost is added to the function that called it, so the totals never change, and a function says `↳ 1.1s of it below, in code not shown` when part of its own cost is there. The hidden code is deeper ops and library code, the decorators of the `charm_tracing` library (`wrapped_function`, `wrap_init`), and the top level of Python modules: code that runs as a module is imported. A stack with nothing left is shown as what it was: `(importing Python modules)`, the time before ops starts, `(ops framework)`, ops work with no charm function on the stack, or `(Python runtime and libraries)`. With all code, the top level of a module is shown as `importing <file>`.

### Call tree

One line per function with its time with calls and its own, indented only where calls branch, with a bar for its share. Functions under 1% of the part you're looking at aren't shown.

### Table

One row per function: `own` is the cost of the function's own code, `with calls` also counts what it called, and the last two columns are the same function's totals for the other two settings of `s`. Functions with no cost of their own are left out. Under the table, the selected function's call path reads from the function out to its callers.

## Memory

`m` in the inspector says what happened to memory around a hook recorded with `--attach unit`, from 2 seconds before the hook until its workloads have settled: the probe's `--linger` time, 30 seconds by default, after the hook or after the last Pebble change it started was ready, whichever is later. It says it in this order:

- **What happened**, a sentence for the hook process and each container: where memory started, where it ended, and how high it got in between. The hook process is only the charm's Python process while it ran the hook. A container is everything running in it, with the file cache the kernel keeps and can take back; the view says how much of a container's memory is file cache.
- **A table** of the memory before the hook, the highest it reached, and after. For the charm container, the highest can come from the probe's own record of the peak, which can be higher than any single reading.
- **Over time**, a line for each container from its lowest reading to its highest, with a lane for the hook (`━`, and `─` for the unit's other hooks) and one for where Pebble changes in the workload containers started (`▲`). The hook process's memory along the hook is in the trace.
- **What took memory in the hook process**, its biggest jumps: how much, when relative to the hook, in what part of it (the charm's `__init__`, an observer, or the process starting up before ops), and the code that was running, innermost first, where `←` means called by.
- **When a container grew**, the jumps of each container, with the Pebble change that was running then, or the last one before it.
- A warning when a container reached its memory limit or had a process killed.

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Scroll. |
| `t`, `p`, `d` | Show the trace, the profile, or the databag and config changes instead. |
| `q`, `Esc` | Go back to the trace. |
