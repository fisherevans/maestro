# maestro

A Claude Code skill that turns the main agent into an orchestrator. The orchestrator does no implementation itself - it spawns sub-agents in isolated git worktrees, tracks them, and handles merging. The point is to keep the main agent's context clean across long iteration sessions and to stay responsive when you pepper in multiple small fixes.

Repo contents:
- `skill/SKILL.md` - the skill that defines orchestrator behavior. Install to `~/.claude/skills/maestro/`.
- `cmd/maestro` - a small Go CLI that holds task state and creates worktrees. Stateful glue so the orchestrator doesn't have to reinvent bookkeeping each session.
- `internal/maestro` - the package the CLI is built on.

## Why both a skill and a CLI

The skill alone would force the orchestrator to track tasks, branches, agent IDs, declared file lists, and merge state in its own context. That's exactly what bloats context across long sessions and what we're trying to avoid. The CLI moves bookkeeping to a deterministic tool the orchestrator just calls.

The CLI is intentionally medium-thin: it manages state and worktrees only. It does not run `git merge`, `git rebase`, or `git pull`. Those steps live in the skill prompt so the orchestrator owns the merge protocol explicitly. This keeps the failure modes obvious: state corruption is a CLI bug, merge mistakes are a skill prompt issue.

## Install

Requires Go on your PATH.

```
git clone <this-repo> maestro
cd maestro
./install.sh
```

`install.sh` builds `maestro` to `~/bin/maestro` and symlinks `skill/` into `~/.claude/skills/maestro/`. Override either with env vars:

```
BIN_DIR=/usr/local/bin SKILL_LINK=$HOME/.config/claude/skills/maestro ./install.sh
```

Restart Claude Code after first install so it picks up the new skill.

### Upgrade

`install.sh` is idempotent. To upgrade:

```
cd maestro
git pull
./install.sh
```

This rebuilds the CLI in place and leaves the skill symlink untouched (since edits flow through the symlink already).

### Uninstall

```
rm "$HOME/bin/maestro"
rm "$HOME/.claude/skills/maestro"
# optionally wipe state and worktrees
rm -rf "$HOME/.maestro"
```

### Manual install

If you'd rather not run the script:

```
go build -o ~/bin/maestro ./cmd/maestro
ln -s "$(pwd)/skill" ~/.claude/skills/maestro
```

## Usage

In a Claude Code session inside a project repo, invoke the skill (`/maestro`) or just describe what you want and ask Claude to operate as the orchestrator. From then on, the agent should:

1. `maestro init --project=<name>` for the current repo if it isn't yet.
2. Set `MAESTRO_PROJECT=<name>` once.
3. Spawn sub-agents per request via the Agent tool, in worktrees the CLI creates.
4. Merge their work back to base when they report done.

You can also drive the CLI yourself.

```
maestro project list
maestro task list
maestro task get t3 --json
maestro conflicts t7
```

State lives at `~/.maestro/<project>/state.json`. Worktrees at `~/.maestro/<project>/wt/<task-id>/`. To wipe a project: `rm -rf ~/.maestro/<project>` and remove any leftover worktree references with `git -C <repo> worktree prune`.

## Commands

```
maestro init --repo=<path> [--base=<branch>] [--force]
maestro project list
maestro project show
maestro task new --description="..." [--base=<branch>]
maestro task list [--status=active|pending|in_progress|...]
maestro task get <id> [--json]
maestro task update <id> [--status=...] [--agent-id=...] [--note=...] [--summary=...] [--commit=...]
maestro task files <id> [--add=a,b] [--remove=a,b] [--set=a,b]
maestro task done <id> [--summary=...] [--commit=...]
maestro task abandon <id> [--note=...]
maestro conflicts <id>
maestro worktree path <id>
maestro worktree cleanup <id> [--force]
```

Most commands need a project. Pass `--project=<name>` or set `MAESTRO_PROJECT`. Pass `--json` to most commands for machine-readable output.

## Task lifecycle

```
pending          created, no agent assigned yet
in_progress      sub-agent is working on it
awaiting_review  sub-agent reported done, not yet merged
merged           merged into base, worktree may or may not be cleaned up
blocked          sub-agent reported needs-info or hit a wall
abandoned        cancelled before merge
```

`active` is a virtual filter that selects everything except `merged` and `abandoned`. Useful for `maestro task list --status=active` to see what's still in flight.

## Known limitations

- Sandboxing is advisory. The skill prompt tells implementer sub-agents to stay in their worktree, but Claude Code has no path-level enforcement, so sub-agents can still drift. Watch for it; reinforce in your prompts; don't let `~50%` slippage rate become acceptable.
- The merge protocol is in the skill prompt, not the CLI. If the orchestrator forgets a step (`--no-ff`, stash protection, branch deletion), nothing catches it.
- No locking. Two `maestro task new` calls racing on the same project could allocate the same ID. In practice the orchestrator is single-threaded, so this hasn't come up.
- Conflict detection is by declared file overlap only. Two tasks that touch the same file in different functions still serialize.
