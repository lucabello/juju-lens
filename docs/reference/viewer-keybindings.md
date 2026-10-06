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
| `Enter` | Events | Open the inspector for the selected event: statuses, relation data changes, and RPCs. |
| `.` | Events | Toggle verbose mode, which shows individual RPCs and hook outcomes hidden by default. See [what the viewer shows](../explanation/rpcs-to-hooks.md#what-the-viewer-shows). |
| `n`, `N` | Events | Jump to the next or previous event on the same unit. |
| `Space` | Status | Show only the selected application or unit in all panes, or remove that filter. Selecting an application includes its units. |
| `f` | Logs | Toggle between following the selected event and scrolling freely. |
| `/` | Events, Logs | Filter the focused pane by text. |
| `r` | Any | Clear the `Space` and `/` filters. |
| `m` | Any | Choose a model, in recordings with more than one. |
| `q`, `Esc`, `Ctrl+C` | Any | Quit. |

## Inspector and model picker

| Key | Window | Action |
|---|---|---|
| `d` | Inspector | Toggle showing changes as a diff. |
| `y`, `Y` | Inspector | Copy the value after or before the change. |
| `Enter`, `q`, `Esc` | Inspector | Close the inspector. |
| `↑`/`k`, `↓`/`j` | Model picker | Move the cursor. |
| `g`/`Home`, `G`/`End` | Model picker | Jump to the first or last model. |
| `Enter`, `Space` | Model picker | Choose the selected model. |
| `q`, `Esc`, `Ctrl+C` | Model picker | Close without changing model. |
