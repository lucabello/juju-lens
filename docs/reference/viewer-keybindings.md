# Viewer keybindings

The viewer (`juju-lens view`) shows two rows: **Events** and **Status**
side by side on top, and **Logs** spanning the full width beneath them,
all synchronised to whichever event is currently selected. `Tab`/`Shift+Tab`
cycle keyboard focus between panes.

## Navigation (any pane)

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Move cursor. |
| `PgUp`/`b`, `PgDn` | Page up/down. |
| `g`/Home, `G`/End | Jump to first/last row. |
| `←`/`h`, `→`/`l` | Scroll a long line horizontally. |
| `w` | Toggle line wrapping instead of horizontal scroll. |
| `Tab`, `Shift+Tab` | Cycle focus between panes. |

## Events pane

| Key | Action |
|---|---|
| `Enter` | Open the inspector for the selected event (statuses, databag diffs, the RPCs behind it). |
| `.` | Toggle verbose mode — reveals raw status changes and relation-scope events folded into hooks by default. See [the hook timeline](../explanation/hook-timeline.md). |
| `n`, `N` | Jump to the next/previous event for the same unit. |
| `d` | Toggle diff mode for databag/state snapshots. |
| `y`, `Y` | Copy the selected event's "after" / "before" value. |

## Logs pane

| Key | Action |
|---|---|
| `f` | Toggle follow (locked to the selected event) vs. free scrolling. |

## Status pane

| Key | Action |
|---|---|
| `space` | Pin/unpin an application or unit row as a scope filter (an app pins all its units). |

## Global

| Key | Action |
|---|---|
| `m` | Open the model picker (recordings with more than one model). |
| `/` | Open a search prompt, scoped to whichever of Events/Logs is focused; filters that pane by substring. |
| `r` | Reset both the scope filter and the search filter. |
| `q`, `Esc`, `Ctrl+C` | Quit. |

## Model picker

Opened with `m`.

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Move cursor. |
| `g`/Home, `G`/End | Jump to first/last model. |
| `Enter`, `Space` | Select. |
| `q`, `Esc`, `Ctrl+C` | Close without changing model. |
