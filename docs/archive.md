# Historical activity archive

Kenn Forge can backfill provider activity into its local SQLite database.
Archive work uses spare provider capacity, so normal sync stays ahead of
historical requests.

Reports read only local data. They do not spend provider requests.

## Coverage

Full archival covers supported historical issues, pull requests or merge
requests, comments, reviews, and inline threads.

The archive mirrors current provider state. It does not retain deleted
content, previous edits, commits, CI history, releases, labels, assignments,
or unrelated lifecycle events.

Provider capabilities determine coverage. Unsupported inventory appears as
partial coverage. GitLab merge-request history is currently partial because
its pagination cannot guarantee a complete traversal across equal timestamps.
GitLab issue history and updated-item maintenance remain available.

## Start

Every configured repository starts in discovery mode. Discovery records item
identities without loading every detail.

Promote one repository or all repositories to full archival:

```sh
kenn-forge archive start --repo 'github|github.com/owner/repo'
kenn-forge archive start --all
```

Progress survives daemon restarts. Repositories advance independently.

## Monitor

```sh
kenn-forge archive status --json
```

Common states:

- **current**: supported inventory and item details are complete.
- **partial**: completed archival has unsupported or inaccessible coverage.
- **running**: archive work is ready or in progress.
- **waiting_for_budget**: live work or provider limits consume the safe budget.
- **paused**: configuration or an operator paused new work.
- **blocked**: work cannot advance until a provider, cursor, access, or contract
  error receives attention.

Errors identify the affected scope without exposing credentials. Removing a
repository pauses its archive state but keeps data and progress. Adding the
same provider and host identity resumes it.

## Pause and resume

```sh
kenn-forge archive pause --all
kenn-forge archive pause --repo 'github|github.com/owner/repo'
```

Pause stops new archive work without deleting data. Start and pause are
idempotent. Starting the same scope resumes durable progress.

There is no manual cursor reset. Invalid cursors, page limits, access loss,
and contract errors block only the affected scope.

## Report

```sh
kenn-forge archive report --days 7
kenn-forge archive report --start 2026-07-01 --end 2026-07-07 --verbose
```

Reports use UTC. `--days` selects rolling 24-hour periods. Date-only ranges
include both dates. RFC3339 ranges use an inclusive start and exclusive end.

Markdown is the default. Use `--format json`, `--output PATH`, and repeated
`--repo` filters for automation. Check status before treating a report as
complete.

See [Commands](commands.md#manage-historical-archives) for the short syntax
reference.
