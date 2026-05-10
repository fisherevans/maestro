# maestro

A Claude Code skill that turns the main agent into an orchestrator. It does no implementation itself - every coding, planning, or review task gets delegated to a sub-agent in an isolated git worktree, and the orchestrator handles merging when they're done.

The problem this solves: long iteration sessions on a project, where you keep peppering the agent with small fixes ("change this label, fix that bug, also can you rename Foo to Bar"). A normal session loses track when you do that. The agent forgets earlier requests, interrupts in-progress work to chase new ones, or ends with 60 modified files in a tangle. Maestro keeps the orchestrator's context lean by pushing implementation, exploration, and even merge plumbing out to sub-agents that report back in 2-4 sentence summaries. You can fire off five things in a row, walk away, come back to a clean merge log.

Concretely, a session looks like:

```
You: fix the login race + rename getFoo to fetchFoo

Orchestrator: dispatched
  t12: fix login race          (background)
  t13: rename getFoo→fetchFoo  (background, declared files don't conflict)

[time passes]

Orchestrator: t12: fix login race merged. Smoke gate passed.
              t13: rename getFoo→fetchFoo merged. Smoke gate passed.

You: how does the login fix actually work?

Orchestrator: [sends message to t12's original sub-agent, who still has context]
              "Wraps the credential check in a sync.Once so concurrent requests
              from the same client don't double-validate. See auth/login.go:84."
```

Sub-agents commit on their own branches but never merge or push. The orchestrator runs the merge protocol via a dedicated merge sub-agent (so the smoke gate's build/test output never enters the orchestrator's context). For follow-up questions on completed work, it routes back to the original implementer via SendMessage. None of this enters the main session's context window beyond the short status reports.

## Layout

- `skill/SKILL.md` - defines orchestrator behavior. Installed at `~/.claude/skills/maestro/`.
- `cmd/maestro` - a small Go CLI that holds task state and creates worktrees. Stateful glue so the orchestrator doesn't have to reinvent bookkeeping each session.
- `internal/maestro` - the package the CLI is built on.

## Why both a skill and a CLI

The skill alone would force the orchestrator to track tasks, branches, agent IDs, declared file lists, and merge state in its own context. That's exactly what bloats context across long sessions and what we're trying to avoid. The CLI moves bookkeeping to a deterministic tool the orchestrator just calls.

The CLI is intentionally medium-thin: it manages state and worktrees only. Merge plumbing lives in the skill prompt and runs via a delegated merge sub-agent. State corruption is a CLI bug; merge mistakes are a skill prompt issue. Failure modes stay separable.

## Install

Requires Go (1.23+) on your PATH and Claude Code.

```
git clone https://github.com/fisherevans/maestro.git
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
maestro init --repo=<path> [--base=<branch>] [--smoke-gate="..."] [--force]
maestro project list
maestro project show
maestro project find --repo=<path>
maestro project update [--smoke-gate=...] [--default-base=...] [--clear-smoke-gate]
maestro project rename --to=<name>
maestro task new --description="..." [--label="..."] [--base=<branch>]
maestro task list [--status=active|pending|in_progress|...]
maestro task get <id> [--json]
maestro task update <id> [--status=...] [--agent-id=...] [--label=...] [--note=...] [--summary=...] [--commit=...]
maestro task files <id> [--add=a,b] [--remove=a,b] [--set=a,b]
maestro task done <id> [--summary=...] [--commit=...]
maestro task abandon <id> [--note=...]
maestro task delete <id> [--keep-worktree] [--force]
maestro conflicts <id>
maestro worktree path <id>
maestro worktree cleanup <id> [--force]
maestro worktree restore <id>
maestro project sweep [--older-than=7d] [--status=merged,abandoned] [--apply] [--keep-worktrees]
maestro statusline [--project=<name>] [--no-project-name]
```

`worktree cleanup` removes the directory but keeps the task record so SendMessage to the original sub-agent still works for follow-up questions. `task delete` removes the record entirely (and the worktree by default). `project sweep` is the bulk version, dry-run by default; suitable for cron or a between-sessions tidy-up.

`project find` is how the orchestrator notices it's been in a repo before. `project rename` requires no active worktrees (worktree paths are absolute and would break). For milestones / phase boundaries, just `maestro init` a new project name pointing at the same repo - multiple projects per repo is supported.

Most commands need a project. Pass `--project=<name>` or set `MAESTRO_PROJECT`. Pass `--json` to most commands for machine-readable output.

## Statusline

`maestro statusline` emits a one-line summary of active tasks. Suitable for Claude Code's `statusLine` setting:

```json
{
  "statusLine": {
    "type": "command",
    "command": "maestro statusline",
    "refreshInterval": 5
  }
}
```

Output looks like `jellybean: 2 in-progress · 1 pending · 1 blocked`. Counts only active statuses (excludes merged and abandoned). Prints nothing when there's no maestro project for the current cwd, so the line is clean when you're working outside an orchestrated repo.

Project resolution order: `--project` flag, then `MAESTRO_PROJECT` env, then auto-detect from cwd via `project find`. If two Claude Code sessions are running in different repos, each session's statusline auto-scopes to its own project; in the rare case of two sessions in the same repo, set `MAESTRO_PROJECT` differently in each shell to disambiguate.

Flags: `--project=<name>` to pin explicitly, `--no-project-name` to omit the project prefix.

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

## Status

Young project. Built and exercised across a handful of real sessions on a multi-component codebase (Go backend + two Vite/React frontends). The skill behaves as designed in those sessions but the surface area is large; expect rough edges.

## Known limitations

- Sandboxing is advisory. The implementer prompt tells sub-agents to stay in their worktree, but Claude Code has no path-level enforcement, so sub-agents can still drift into the parent repo. The hard rules in the prompt template have brought the slippage rate down substantially in observed sessions, but it's not zero. Reinforce in your prompts; don't soften the rules.
- The merge protocol lives in the skill prompt (executed by a delegated merge sub-agent). If the merge sub-agent skips a step (`--no-ff`, stash protection, branch deletion, smoke gate), nothing in the CLI catches it. The skill prescribes the protocol explicitly; failures show up as messy git history, not silent corruption.
- No locking on state writes. Two `maestro task new` calls racing on the same project could allocate the same ID. The orchestrator is single-threaded so this hasn't come up.
- Conflict detection is by declared file overlap only. Two tasks that touch the same file in different functions still serialize.
- `project rename` refuses while any worktrees exist (worktree paths are absolute and would break). Use the milestone-fork pattern instead - just `maestro init` a new project name on the same repo.

## Contributing

Issues and PRs welcome. The codebase is small (~1500 lines Go + ~350 lines markdown skill). The skill prompt is the highest-leverage surface; if you have ideas on tightening orchestrator behavior, that's the place. The CLI is intentionally bounded; new features should justify the additional protocol surface.
