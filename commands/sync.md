---
description: Synchronize issues with git remote
argument-hint: [--dry-run] [--message]
---

Synchronize issues with git remote in a single operation.

## Sync Steps

1. Export pending changes to JSONL
2. Commit changes to git
3. Pull from remote (with conflict resolution)
4. Import updated JSONL
5. Push local commits to remote

Wraps the entire git-based sync workflow for multi-device use.

## Usage

- **Basic sync**: `bd sync`
- **Preview**: `bd sync --dry-run`
- **Custom message**: `bd sync --message "Closed sprint issues"`
- **Pull only**: `bd sync --no-push`
- **Push only**: `bd sync --no-pull`

## Note

Most users should rely on the daemon's automatic sync (`bd daemon --auto-commit --auto-push`) instead of running manual sync. This command is useful for one-off syncs or when not using the daemon.
