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

Project lifecycle:
```
maestro init --repo=<path> [--base=<branch>] [--smoke-gate="..."] [--force]
maestro project list
maestro project show
maestro project find --repo=<path>
maestro project update [--smoke-gate=...] [--default-base=...] [--clear-smoke-gate]
maestro project rename --to=<name>
maestro project sweep [--older-than=7d] [--status=abandoned] [--include-merged] [--apply]
```

Tasks:
```
maestro task new --description="..." [--label="..."] [--tags=a,b] [--session=<id>] [--prompt-stdin | --prompt-file=<path>]
maestro task get-prompt <id>
maestro task report <id> [--source=agent] [--file=<path>]  # validated JSON on stdin
maestro task list [--status=active|pending|in_progress|...]
maestro task get <id> [--json]
maestro task update <id> [--status=] [--agent-id=] [--label=] [--note=] [--note-content-stdin --note-source= --note-type=] [--add-tags=] [--remove-tags=] [--summary=] [--commit=]
maestro task files <id> [--add=a,b] [--remove=a,b] [--set=a,b]
maestro task done <id> [--summary=...] [--commit=...]
maestro task abandon <id> [--note=...]
maestro task delete <id> [--keep-worktree] [--force]
```

Sessions and history:
```
maestro session start [--name=...]
maestro session list [--include-condensed]
maestro session get <id>
maestro session current
maestro session pending-condense <id>
maestro session condense <id> --apply --summary-stdin
maestro tag list [--with-counts]
maestro tag rename --from=<old> --to=<new>
maestro search [--text=] [--tag=a,b] [--session=] [--status=] [--since=] [--until=] [--limit=20] [--full]
```

Coordination and display:
```
maestro conflicts <id>
maestro worktree path <id>
maestro worktree cleanup <id> [--force]
maestro worktree restore <id>
maestro statusline [--project=<name>] [--no-project-name]
maestro status [--project=<name>] [--last-merged=N]
maestro web [--port=9876] [--bind=127.0.0.1] [--open=true]
```

Notes:
- `worktree cleanup` removes the directory but keeps the task record (carries summary/commit/agent_id for follow-up questions). `task delete` removes the record entirely (rare during sessions; loses history).
- `project sweep` defaults to abandoned tasks only - merged tasks are kept as durable history; condense them via `session condense`. Pass `--include-merged` to override.
- `project rename` requires no active worktrees (paths are absolute and would break). For milestones, prefer `maestro init` a new project name - multiple projects per repo is supported.

Most commands need a project. Pass `--project=<name>` or set `MAESTRO_PROJECT`. Pass `--json` to most commands for machine-readable output.

## Review, verify, merge

When an implementer reports done, the merge sub-agent doesn't just integrate - it runs three phases:

- **REVIEW**: substantive read of the diff. Flags design issues, missed edge cases, hidden coupling, even when the code is functional. Blocking findings halt the merge; non-blocking findings are bubbled up to the user in the completion summary.
- **VERIFY**: confirms the implementation actually matches the task description and the implementer's reported summary. Catches "built something subtly different from what was asked" cases.
- **MERGE**: the existing stash/merge/smoke/cleanup sequence. Only runs if REVIEW and VERIFY both pass.

Findings persist as Notes (`Type=review`) so search and condensation can surface them later. Skill teaches the orchestrator to relay findings to the user verbatim when REVIEW blocks, and to summarize accepted non-blocking concerns in the completion summary.

## Orchestrator-user dialogue

The skill structures every orchestrator turn around three things the user cares about:

- **Rephrase the ask at dispatch** so the user can course-correct before sub-agents burn tokens. Embedded in the same message that announces the spawn ("got it - fixing the login race in auth/login.go. dispatching t14.").
- **Substantive completion summary** when work merges: a PR-description-style writeup covering strategy, design decisions, trade-offs, what was deferred, what review caught. Not a file list or diff stat - the user can read the diff if they want.
- **End-of-turn signal** every substantive turn closes with `**IN PROGRESS:** ...` (active tasks listed) or `**NOW IDLE.**` (everything handed back). Bolded so the user can find it at a glance.

The middle (file edits, iteration noise) stays out of the user-facing layer. Only actionable items interrupt: REVIEW pushback, smoke failures, needs-info from a sub-agent.

## Structured reports

Sub-agents file their final reports via `maestro task report <id>` with a validated JSON body on stdin:

```json
{
  "status": "done",
  "summary": "rewired credential check to use sync.Once for client dedup.",
  "files": ["auth/login.go", "auth/login_test.go"],
  "commit": "deadbeef12345",
  "deferred": ["token refresh path has the same shape - left for a follow-up"],
  "concerns": ["sync.Once keyed by clientID only; stale credentials could coalesce"],
  "review_findings": [
    {"severity": "non-blocking", "title": "consider keying by clientID+credential", "file": "auth/login.go", "line": 84}
  ],
  "notes": "tests verify both concurrent and serial paths."
}
```

The CLI validates required fields (`status`, `summary`), enforces a `severity` enum on review findings (`blocking | non-blocking`), and rejects unknown fields - typos surface as errors instead of silent drops. The orchestrator and merge sub-agent file the same shape (the schema covers `merge_commit`, `verify_notes`, `smoke_tail`, and `review_findings` for the merge side). The web UI parses the stored JSON and renders structured fields (colored status pill, callout lists for deferred/concerns, severity-colored cards for review findings, monospace commits). Legacy text-format reports from older sessions still parse via a fallback.

## Knowledge store

Tasks in maestro are durable. They outlive the session that created them and accumulate into a searchable record of what was asked, what was done, why, and what was decided. The orchestrator uses this:

- **Search before creating** new tasks (`maestro search --tag=auth` before opening a new auth-related task) - catches prior decisions and constraints without re-deriving from code.
- **Implementer prompts are stored** via `task new --prompt-stdin`. Sub-agents fetch them with `task get-prompt`. The orchestrator's context never holds the long body twice.
- **Reports are written by sub-agents** via `task update --note-content-stdin --note-type=report`. The orchestrator reads only what it needs via `task get` or `status`.
- **Sessions** group related work. Multiple sessions can run concurrently on the same project. At natural boundaries (focus transitions, milestones), the orchestrator proposes `session condense`: it summarizes the session into a single condensed entry and trims each task's verbose fields. Metadata stays so search keeps working; the verbose noise drops.
- **Tags** emerge organically. `tag list` enumerates them; `tag rename` canonicalizes drift.

The skill's "What you never do" includes: never inline the implementer's full prompt twice; never inline a sub-agent's verbose report in your context; never skip `maestro search` before creating a task in an area you've worked in before; never auto-condense without proposing.

## Web UI

`maestro web` runs a local browser UI for exploring projects, sessions, tasks, condensed summaries, and the full implementer prompt / exchange log that the CLI also reads. Read-only; intended for the human reviewer (you), not for the agent.

```
maestro web                       # http://127.0.0.1:9876, opens browser
maestro web --port=9000 --open=false
maestro web --bind=0.0.0.0        # bind beyond localhost (not recommended)
```

Blocks until ctrl-C. Pages:

- `/` - project list, with task/session counts and last activity
- `/p/<project>` - project detail: active sessions, active tasks, recent merges, tag cloud
- `/p/<project>/s/<session>` - session view: tasks in the session, plus condensed summary if condensed
- `/p/<project>/t/<task>` - full task: description, tags, declared files, summary, the report/review/decision notes, and the implementer prompt (collapsed by default)
- `/p/<project>/search` - filter by text, tag, session, status, date range

State lives in the same `~/.maestro/<project>/state.json` the CLI reads, so changes from active orchestrator sessions show up on refresh. The server is stdlib-only (`net/http` + `html/template` + `embed`), single binary, no JS framework.

## Status snapshot

`maestro status` prints a multi-line snapshot of active tasks (sorted by status priority, with ages) and the last few merges. The format is tight enough that the orchestrator can run it and let the output stand without re-narrating in prose, which keeps status checks cheap on context.

```
jellybean

  t8   in_progress      on-screen keyboard for library search       (2m)
  t9   pending          auto-off override fix                       (15s)
  t10  blocked          admin override modal accessibility          (5m)

Recently merged (last 3):
  t7   playback reliability cluster                                 (4m ago)
  t6   nextup into cw resolver                                      (8m ago)
  t5   browse hero detail                                           (12m ago)
```

Project resolution: `--project` flag, then `MAESTRO_PROJECT`, then cwd auto-detect. `--last-merged=N` controls how many recent merges to show (default 3, `0` to omit). `--json` for machine-readable output.

## Statusline (optional)

`maestro statusline` emits a one-line summary of active tasks (e.g. `jellybean: 2 in-progress · 1 pending · 1 blocked`). Suitable for Claude Code's `statusLine` setting.

`install.sh` does not configure this. Claude Code only has one `statusLine` slot, and replacing whatever you already have (e.g. ccstatusline) without asking would be rude. Opt in via:

```
./enable-statusline.sh             # print the snippet, change nothing
./enable-statusline.sh --apply     # write to ~/.claude/settings.json (with timestamped backup)
./enable-statusline.sh --remove    # delete the statusLine key
```

`--apply` and `--remove` need `jq`. `--apply` makes a `.bak.<timestamp>` of your current settings before writing.

Output behavior: counts only active statuses (excludes merged and abandoned). Prints nothing when there's no maestro project for the current cwd, so the line stays clean outside orchestrated repos.

Project resolution order: `--project` flag, then `MAESTRO_PROJECT` env, then auto-detect from cwd via `project find`. Multiple Claude Code sessions in different repos each auto-scope to their own project; sessions in the same repo can disambiguate with `export MAESTRO_PROJECT=...`.

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

## Developing

For dataflow, on-disk layout, the data model, design decisions, dev workflow, and step-by-step recipes for adding a CLI subcommand / a web page / a skill section, see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Aimed at a human or agent picking the codebase up cold; ~400 lines, shortens the first-hour grep.

## Contributing

Issues and PRs welcome. The codebase is small (~3000 lines Go + ~600 lines markdown skill + ~500 lines HTML/CSS). The skill prompt is the highest-leverage surface; if you have ideas on tightening orchestrator behavior, that's the place. The CLI is intentionally bounded; new features should justify the additional protocol surface. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the lay of the land before diving in.
