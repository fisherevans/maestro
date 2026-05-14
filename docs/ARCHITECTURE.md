# Architecture

How the pieces of maestro fit together, for someone (human or agent) picking the codebase up cold and intending to iterate on it.

The user-facing docs (`README.md`, `skill/SKILL.md`) describe *what* the tool does. This doc describes *how it's built* and *how to change it*.

## System overview

Maestro has three runtime pieces that all talk to the same on-disk state:

```
                ┌──────────────────────────────────────┐
                │ Claude Code orchestrator             │
                │ (regular Claude session, with the    │
                │  maestro skill loaded by /maestro)   │
                └────────────────┬─────────────────────┘
                                 │ spawns via Agent tool
                ┌────────────────▼─────────────────────┐
                │ Sub-agents (Planner / Implementer /  │
                │ Review-verify-merge / Reviewer)      │
                └────────────────┬─────────────────────┘
                                 │ shell out to `maestro` CLI
                ┌────────────────▼─────────────────────┐
                │ maestro CLI (Go single binary)       │
                │   — load state, mutate, save         │
                │   — create / clean worktrees         │
                │   — validate JSON reports            │
                └────────────────┬─────────────────────┘
                                 │ reads/writes
                ┌────────────────▼─────────────────────┐
                │ ~/.maestro/<project>/state.json      │
                │ ~/.maestro/<project>/wt/<task>/      │
                │   (git worktree on maestro/<task>)   │
                └────────────────┬─────────────────────┘
                                 │ reads (read-only)
                ┌────────────────▼─────────────────────┐
                │ maestro web (local server)           │
                │ http://127.0.0.1:9876                │
                │   — explore projects, sessions,      │
                │     tasks, condensed summaries       │
                │   — search across history            │
                └──────────────────────────────────────┘
```

The CLI is the *only* writer to state. The web UI is read-only. Agents never touch state.json directly; they always go through the CLI. This keeps state mutations validated, atomic, and concurrency-safe (the CLI uses tmp+rename for writes).

## On-disk layout

```
~/.maestro/
├── <project-a>/
│   ├── state.json                  # single JSON file per project
│   └── wt/
│       ├── t1/                     # git worktree on branch maestro/t1
│       ├── t2/
│       └── ...
└── <project-b>/
    ├── state.json
    └── wt/...
```

State writes are atomic: `Store.Save` writes to `state.json.tmp` then renames over `state.json` (see `internal/maestro/state.go`).

Each worktree is a real `git worktree add`-managed directory; agents `cd` in and commit on `maestro/<task-id>`. Worktrees get cleaned by the merge sub-agent (`maestro worktree cleanup`); task records survive.

## Data model

Five core types live in `internal/maestro/state.go`. Relationships:

```
State
 ├─ Project           — one per project; carries name, repo path, default base, smoke gate, ID counters
 ├─ []*Task           — one per logical unit of work, scoped to a Session
 │    ├─ Session str  — foreign key to Session.ID
 │    ├─ Tags []str
 │    ├─ ImplementerPrompt str    — the long body fetched via `maestro task get-prompt`
 │    ├─ DeclaredFiles []str      — what the implementer expected to touch
 │    ├─ Notes []Note             — chronological audit log
 │    │    ├─ Source: orchestrator | agent | user | system
 │    │    ├─ Type:   report | exchange | fold | decision | review | system
 │    │    └─ Content: text OR canonical JSON (for type=report)
 │    └─ ... (Status, Branch, BaseCommit, Summary, FinalCommit, CondensedAt, etc.)
 └─ []*Session        — units of contiguous work; condensation lives here
      ├─ StartedAt
      ├─ EndedAt      — set when condensed
      └─ Condensed    — the orchestrator-written summary text
```

**Report** is a separate schema (not a stored type itself) representing the canonical JSON shape for a `type=report` Note. Validated by `maestro task report`. Stored as the Note's Content; rendered back to structured fields by the web UI.

**Side-effects**: when a Report carries a `commit` or `summary`, `cmdTaskReport` mirrors them onto `Task.FinalCommit` / `Task.Summary` so other commands (status, list) show useful info without re-parsing the latest Note.

## Code organization

```
cmd/maestro/main.go              CLI entry + dispatch + every command implementation.
                                 ~1700 lines. One large file by choice: every command is
                                 a `cmdFoo(args []string) error` function, dispatched by
                                 string-match in the run() / cmdTask() / cmdSession() / etc.
                                 chains. Easy to grep, easy to follow.

internal/maestro/state.go        Types (State, Project, Task, Note, Session, Report) +
                                 Store (Load/Save) + all helpers (FindTask, SearchTasks,
                                 AllTags, RemoveTask, etc.). The only file that owns the
                                 wire format of state.json.
internal/maestro/git.go          Thin shell around `git` subprocess. CreateWorktree /
                                 AttachWorktree / RemoveWorktree / BranchExists /
                                 ResolveSHA. No state knowledge.

internal/maestro/web/web.go      `Serve(addr, openBrowser)` + template func map + helper
                                 utilities (humanizeAgo, sortedTagCounts).
internal/maestro/web/handlers.go HTTP handlers + per-page view-model structs.
internal/maestro/web/render.go   Markdown rendering (goldmark), report-note parsing (JSON
                                 first, legacy text key:value second, plain markdown
                                 third), search-snippet generation with <mark>.
internal/maestro/web/templates/  Server-rendered HTML pages. Each is self-contained
                                 (no inheritance), uses template funcs from web.go.
internal/maestro/web/static/     style.css and rows.js, all embedded via embed.FS.

skill/SKILL.md                   Operating rules for the orchestrator agent. Loaded
                                 by Claude Code at /maestro. Edits flow through
                                 immediately because install.sh symlinks this dir into
                                 ~/.claude/skills/maestro/.

install.sh                       Idempotent installer: builds the binary, symlinks the
                                 skill, prints PATH hints. Overrides via BIN_DIR and
                                 SKILL_LINK env vars.
enable-statusline.sh             Opt-in Claude Code statusLine wiring. Default mode
                                 prints the snippet; --apply edits settings.json with
                                 a timestamped backup.

docs/ARCHITECTURE.md             (this file)
README.md                        User-facing features and CLI reference.
LICENSE                          MIT.
```

The codebase is small (~3000 lines Go, ~600 lines skill, ~500 lines HTML/CSS). Reading the code is feasible; this doc is for shortening that first hour.

## Key design decisions

**Stdlib-first.** The only third-party dependency is `goldmark` for markdown rendering, picked because it's the de-facto Go markdown library and gives us safe HTML escaping for free. Everything else is `net/http`, `html/template`, `embed`, `encoding/json`, `os/exec`, etc. No web framework, no CLI framework, no router library.

**Single binary.** Templates and CSS live under `embed.FS`. `go build ./cmd/maestro` produces one executable that runs the CLI, the web UI, and everything in between.

**CLI is the only mutator.** Agents never touch `state.json` directly. The web UI is read-only. This keeps mutations validated and atomic - if someone needs a new way to mutate state, they add a CLI command for it.

**Project = scope, not a 1:1 mapping with a repo.** Multiple maestro projects can target the same repo (the "milestone fork" pattern). Project names are logical labels.

**State is authoritative.** Agents don't keep their own mental task list; they read state every command. The skill explicitly says: don't use TodoWrite-style tools to duplicate maestro tracking.

**Atomic writes.** `Store.Save` writes to `state.json.tmp` then renames. If the process dies mid-write, the old state survives.

**Medium-thin CLI.** The CLI manages state and worktrees. It does NOT run `git merge`, `git rebase`, or `git pull` - those live in the skill's merge sub-agent prompt. This keeps failure modes separable: state corruption is a CLI bug; merge mistakes are a skill prompt issue.

**Tasks are durable; condense, don't delete.** `project sweep` defaults to abandoned tasks only. Merged tasks stay as knowledge-store entries. Condensation is the mechanism for trimming verbose fields while preserving searchability.

**Structured reports with validation.** `maestro task report` reads JSON, validates the schema, and rejects unknown fields. Typos surface as errors instead of silent drops. The shape supports both implementer fields (files, deferred, concerns) and merge sub-agent fields (merge_commit, review_findings, smoke_tail) in one schema.

**No background daemon.** Maestro is purely a CLI + a local-only web server. Nothing runs in the background; nothing auto-merges or auto-promotes. Every action happens because an agent or human ran a command. (The skill explicitly calls this out in "Things gone wrong" - prior sessions confused "maestro" the CLI with a daemon, and stalled.)

## Development workflow

```bash
# build
go build ./...

# install locally (rebuilds + symlinks the skill into ~/.claude/skills/maestro)
./install.sh

# run the web UI against your current state
maestro web

# vet
go vet ./...
```

### Smoke testing

There is no test framework wired up yet (intentional - the project is young and the behavior shape has been moving). Verification has been improvised inline in commit messages and in scratch directories:

```bash
TMP=$(mktemp -d)
cd "$TMP"
git init -q -b main && git commit --allow-empty -q -m initial
go -C /Users/fisher/scratch/maestro build -o /tmp/maestro ./cmd/maestro
rm -rf ~/.maestro/smoketest
export MAESTRO_PROJECT=smoketest
/tmp/maestro init --repo="$TMP" --base=main >/dev/null
/tmp/maestro session start --name=test  # capture the s1 ID
export MAESTRO_SESSION=s1
# ... exercise the change ...
rm -rf "$TMP" ~/.maestro/smoketest
```

For larger changes, the existing live `~/.maestro/jellybean/` directory is a real read-only target: `MAESTRO_PROJECT=jellybean maestro status` works as a load test for schema/render changes.

When adding tests properly (`go test ./...`), suggestions:
- table-driven for state.go helpers (SearchTasks, ConflictingTasks, AllTags)
- httptest-driven for handlers.go
- Smoke for the CLI dispatch end-to-end

## How to add a new CLI subcommand

1. **Pick the dispatch chain.** Top-level commands (like `init`, `web`) go in `run()`. Sub-commands of an existing group (like `task delete`) go in the appropriate sub-dispatcher (`cmdTask`, `cmdSession`, `cmdProject`, `cmdTag`, `cmdWorktree`).

2. **Implement `cmdFoo(args []string) error`.** The pattern is consistent:

   ```go
   func cmdFoo(args []string) error {
       fs := flag.NewFlagSet("foo", flag.ContinueOnError)
       project := fs.String("project", "", "project name")
       // ... other flags ...
       if err := fs.Parse(args); err != nil {
           return err
       }
       store, st, err := loadState(*project)
       if err != nil {
           return err
       }
       // ... mutate st ...
       if err := store.Save(st); err != nil {
           return err
       }
       // print confirmation or writeJSON()
       return nil
   }
   ```

   For commands that take a positional task ID, use `parseFlagsWithID(fs, args)` instead of `fs.Parse(args)` (handles interleaved flag/positional ordering).

3. **Add usage line** in the `usage` const at the top of `main.go`.

4. **Add helper methods** in `internal/maestro/state.go` if you need new operations on the data model. Keep them as methods on `*State` or `*Task`.

5. **Smoke test** in a scratch directory (see above).

6. **Update README.md** if the command is user-facing.

## How to add a new web page

1. **View model struct** in `handlers.go`: `type fooData struct { ... }`.

2. **Handler function** in `handlers.go`: `s.handleFoo(w, r)`. Load state, build the view model, call `s.render(w, "foo.html", view)`.

3. **Register the route** in `web.go`'s `mux.HandleFunc("GET /path/{param}", s.handleFoo)`.

4. **Template** at `internal/maestro/web/templates/foo.html`. Self-contained HTML; no template inheritance (we use partials via template funcs instead - see `markdown`, `parseReport`, `tokens`, etc.).

5. **CSS** as needed in `static/style.css`. Conventions: status pills use `.pill .status-*`, cards use `.card`, tables use `table.rows` (nested ones add `.nested`).

6. **embed.FS picks it up** on rebuild; just `go build`.

7. **rows.js** is already loaded by every page. Any `<tr data-href="...">` becomes clickable for free.

## How to edit the skill safely

`skill/SKILL.md` is loaded by Claude Code at `/maestro`. It is the orchestrator's operating system prompt. Edits flow through immediately because `install.sh` symlinks `skill/` into `~/.claude/skills/maestro/` - no rebuild needed.

**Structure:**

- Setup: project find / init / session start / smoke-gate detection
- Operating loop: request classification (new / fold / interrupt / queue)
- Sub-agent prompt templates: verbatim text used by the orchestrator to spawn implementer / merge / planner agents
- Communication rules: rephrase, quiet middle, substantive summary, end-of-turn signal
- Decision rules, follow-up patterns, consultation patterns
- Cleanup posture (condense don't delete)
- Things that have gone wrong before
- What you never do

**Guidelines when editing:**

- Hard rules (`Your first action is cd <worktree>`, `Never inline the prompt twice`, etc.) are load-bearing. Don't soften them; sub-agents drifted ~50% of the time before they existed.
- "Things gone wrong" is the tribal-knowledge bucket. When you find a new failure mode in production, capture it here. The orchestrator reads this and avoids the same trap next time.
- Cross-reference: if you change a prompt template (e.g., the merge sub-agent), check that the section describing how to interpret its output is still aligned.
- The skill is markdown, not prompt-engineered XML. Plain English is fine.

## Glossary

**Project** — a scoped directory under `~/.maestro/<name>/` tracking one repo's orchestration history. Long-lived; spans many sessions and agents. One Go struct, one state.json.

**Session** — a unit of contiguous work within a project, bounded by focus or time. Has start/end timestamps. When "condensed," tasks in it have their verbose fields trimmed; `Session.Condensed` carries the canonical history.

**Task** — an atomic unit of work assigned to a sub-agent. Carries label, description, tags, implementer_prompt, notes, status, branch info, final_commit, etc. The basic citizen of the knowledge store.

**Note** — a timestamped log entry on a task. `Type` classifies the entry (`report`, `exchange`, `review`, `decision`, `fold`, `system`). `Content` can be plain text (for most types) or canonical JSON (for `type=report`).

**Report** — the structured JSON schema for a `type=report` Note. Filed via `maestro task report` with validation. Schema covers both implementer fields (files, deferred, concerns) and merge sub-agent fields (merge_commit, review_findings, smoke_tail).

**Worktree** — per-task git working directory at `~/.maestro/<project>/wt/<task-id>/`, on branch `maestro/<task-id>`. Created when a task is created; cleaned by the merge sub-agent after merge.

**Condensation** — the process of trimming a session's verbose notes and implementer prompts into a single summary on `Session.Condensed`, preserving task metadata (label, summary, final_commit, tags) for searchability. Orchestrator-driven via `maestro session condense --apply --summary-stdin`.

**Smoke gate** — a shell command per project (run after every merge) verifying the build / test / typecheck for all components. Stored on `Project.SmokeGate`. Auto-detected by the orchestrator at project init from CLAUDE.md, README, build manifests, and CI workflow files.

**Orchestrator** — the main Claude Code agent operating under the maestro skill. Does no implementation; delegates everything to sub-agents.

**Sub-agent** — a Claude agent spawned via the Agent tool by the orchestrator. Types: implementer (does the actual coding work), planner (explores and produces a plan), review-verify-merge (the three-phase integration agent), reviewer (deeper review for high-risk changes), conflict-resolution (merge conflict handling).

**Condensed task** — a task whose session has been condensed; its `ImplementerPrompt` is dropped and `Notes` are trimmed to keep only `decision` and `report` types (the latter truncated to 200 chars). Metadata stays.

**Active task** — a task in any of: pending, in_progress, awaiting_review, blocked. (Excludes merged and abandoned.) Counted toward the orchestrator's concurrency cap of 3.

**Orphan task** — an active task with no session, or whose session has been condensed. Only legacy data should land here, since `maestro task new` now requires a session.

**Schema-self-describing** — every structured input has a `--schema` discoverability flag. Currently: `maestro task report --schema` prints the full JSON shape + worked examples. Error messages from validation point at it.
