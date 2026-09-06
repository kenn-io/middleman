# Workflows

## Scan recent activity

Open **Activity** for the daily queue. Filter by time, event type, repository,
item type, text, closed state, or bot activity.

Threaded mode groups events by pull request or issue. Flat mode keeps exact
event order. Select a row to open its detail without leaving the queue.

Use **Sync current repository** when one repository needs fresh data. This
avoids scheduling a global refresh.

See [Follow activity across repositories](workflows/activity.md) for saved
filter URLs, local workspace activity, commit diffs, and detail panes.

## Follow a role-based guide

- [Triage an issue](workflows/issue-triager.md)
- [Review a pull request](workflows/code-reviewer.md)

## Move quickly

Use the sidebar for modes and the repository selector for scope. Open the
command palette with `Cmd/Ctrl+K` or `Cmd/Ctrl+P`. Use `Cmd/Ctrl+Shift+K`
while a terminal has focus.

Palette prefixes narrow results:

- `>` for commands
- `pr:` for pull requests
- `issue:` for issues

Press `?` for shortcuts in the current view.

## Review and merge

Open **Pulls**, then select an item. The detail view combines description,
discussion, CI, branch state, review state, changed files, and provider
actions.

Use the **View** menu to filter the timeline or compact rows. Open the file
tree for line-by-line review. Comment, approve, mark ready, close, reopen, or
merge when the provider and credential allow it.

Unsupported actions remain visible but unavailable.

Detected stacks show member order. Mid-stack merges stay blocked by default
until earlier members land.

The conversation, files, and workspace can share a pane layout. Reorder,
split, resize, hide, or maximize panes as the task changes.

## Run manual provider workflows

Actions is opt-in. Open **Settings → Visible modes**, enable **Actions**, and
save. In **Actions**, choose a repository and a manual workflow, select a Git
ref, complete its typed inputs, and run it.

Recent runs show their status, jobs, and steps. Follow the provider link on a
run to inspect its full logs. Pull requests also offer available workflows in
the **Actions** menu: open same-repository pull requests start from the head
branch, while fork and merged pull requests start from the target branch.

## Track local pull-request state

Set a workflow status from the pull-request detail and filter the list by one
or more statuses. This status stays in Kenn Forge. It does not change provider
labels, milestones, projects, or fields.

## Work with issues

Open **Issues** to search, filter, comment, star, close, or reopen issues.
Create a workspace when an issue is ready for implementation.

## Browse repository source

Open **Repos**, choose a repository card, or use **View repository source** in
the command palette. Switch among branches and tags, filter the path tree, and
read a selected file as source. Markdown files also offer a rendered preview,
and the history rail shows commits for the selected file.

The URL records the repository, ref, path, and preview mode, so copied links and
browser back/forward navigation restore the same view. Provider links return to
the original repository when needed.

## Work in local sessions

Create a workspace from a pull request, issue, Kata task, or the **New
workspace** action. A workspace creates a worktree. Choose an agent from the
action menu to create and launch in one step.

New workspaces can start from any tracked repository. Choose a branch name or
let Forge create one from the default branch.

Workspaces use tmux-backed sessions for durable attachment. Launch more shells
or agents from the workspace header. Promote a session into the detail layout
when you want it beside discussion or files.

Install lifecycle hooks to show agent activity in workspace rows:

```sh
kenn-forge agent-hook install
```

Use `--agent NAME` to limit installation. Active work, approval requests, and
input requests update while the sidebar is open. Hook reports expire after 30
minutes without another event, then fall back to tmux activity.

See [Work in local sessions](workflows/workspaces.md) for workspace types,
session layouts, tmux attachment, phone use, deletion, and recovery.

## Use Kata tasks

Link a Kata issue from a pull request, provider issue, or workspace to view its
read-only detail inline. Use **New workspace → Kata issue** to search a selected
daemon and create or reopen a mapped workspace. If Forge cannot match the
Kata project to a configured repository, open **Settings → Kata mappings** and
add an override. Open the task in Kata when you need to edit it.

Kata remains the source of truth for task data. Forge owns only the links
to pull requests, issues, and workspaces. There is no separate Kata mode.

## Review Roborev jobs

Start the Roborev daemon, then open **Reviews**. Filter the queue by repository,
branch, status, or Git ref. Select a job to read the review, inspect its log and
prompt, add a comment, or use the actions available for its current state.

See [Integrations](integrations.md#review-roborev-jobs) for endpoint setup and a
walkthrough of the Reviews page.

## Browse and edit Docs

Enable Docs mode and register Markdown folders. Browse, search, read, edit,
pull, and publish files from the console. Task references can open a Kata task
through the folder's daemon binding.

Files remain on disk inside the configured folders.

See [Read and edit local Docs](workflows/docs.md) for search, file editing, Git
pull and publish, and Kata reference behavior.

## Use a fleet

A hub combines snapshots from enrolled Forge spokes. Its workspace list shows
which node owns each workspace, and **New workspace** can create repository
work on the hub or a writable spoke. A spoke keeps workspace creation and its
workspace list local.

Supported actions route back to the machine that owns the resource, and
terminal traffic uses the spoke's WebSocket API.

Each daemon must have a reachable HTTPS origin. Forge does not use SSH for
fleet transport or daemon startup. See [Federated fleet](federated-fleet.md).
