// maestro is a small CLI helper for the maestro orchestration skill. It
// tracks tasks and worktrees for a single project and gives the orchestrator
// agent a deterministic substrate so it doesn't have to reinvent state
// management every session.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fisherevans/maestro/internal/maestro"
	"github.com/fisherevans/maestro/internal/maestro/web"
)

const usage = `maestro - state and worktree helper for the maestro orchestration skill

Usage:
  maestro <command> [flags]

Project commands:
  init                       Initialize a project (creates ~/.maestro/<project>/)
  project list               List all initialized projects
  project show               Show current project config
  project find --repo=<path> List projects whose repo matches <path>
  project update             Update smoke gate or default base branch
  project rename --to=<name> Rename the current project (requires no active worktrees)
  project sweep              Bulk-delete old completed tasks (dry run unless --apply)

Task commands:
  task new                   Create a task and worktree
  task list                  List tasks for a project
  task get <id>              Show one task
  task get-prompt <id>       Print the stored implementer prompt for a task
  task report <id>           Append a structured (validated JSON) report note
  task update <id>           Update task fields
  task files <id>            Manage declared file list
  task done <id>             Mark a task merged
  task abandon <id>          Mark a task abandoned

Coordination:
  conflicts <id>             Show declared-file overlap with other active tasks
  worktree path <id>         Print absolute path of a task's worktree
  worktree cleanup <id>      Remove a task's worktree (keep the task record)
  worktree restore <id>      Re-create a worktree from its branch (recovery)
  task delete <id>           Delete a task record (and its worktree by default)

Display:
  statusline                 One-line task summary, suitable for a Claude Code statusLine
  status                     Multi-line snapshot: active tasks + recently merged
  web                        Run a local web UI for browsing project/session/task history

Sessions and history:
  session start              Start a session (returns ID; export MAESTRO_SESSION)
  session list               List sessions (active + condensed)
  session get <id>           Show session metadata + tasks in it
  session current            Print MAESTRO_SESSION or "no current"
  session pending-condense <id>   Dump tasks-in-session for orchestrator summarization
  session condense <id>      Apply a condensed summary; trim verbose fields
  tag list                   Enumerate all tags in use, with counts
  tag rename --from --to     Bulk-rename a tag across the project
  search                     Query tasks by text/tag/session/since/until/status

Project scope:
  Most commands need a project. Pass --project=<name> or set MAESTRO_PROJECT.

Output:
  Default output is key: value lines. Pass --json for JSON.

Run any command without arguments to see its flags.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "maestro:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "init":
		return cmdInit(rest)
	case "project":
		return cmdProject(rest)
	case "task":
		return cmdTask(rest)
	case "conflicts":
		return cmdConflicts(rest)
	case "worktree":
		return cmdWorktree(rest)
	case "statusline":
		return cmdStatusline(rest)
	case "status":
		return cmdStatus(rest)
	case "session":
		return cmdSession(rest)
	case "tag":
		return cmdTag(rest)
	case "search":
		return cmdSearch(rest)
	case "web":
		return cmdWeb(rest)
	default:
		return fmt.Errorf("unknown command %q (run `maestro` for usage)", cmd)
	}
}

// resolveProject pulls the project name from the flag, falling back to env.
func resolveProject(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("MAESTRO_PROJECT"); env != "" {
		return env, nil
	}
	return "", errors.New("project required: pass --project=<name> or set MAESTRO_PROJECT")
}

// loadStore resolves the project and opens its store. Use this when the
// project is required but doesn't have to exist yet.
func loadStore(flagVal string) (*maestro.Store, error) {
	name, err := resolveProject(flagVal)
	if err != nil {
		return nil, err
	}
	return maestro.NewStore(name)
}

// loadState is the common path for commands that need an initialized project.
func loadState(flagVal string) (*maestro.Store, *maestro.State, error) {
	store, err := loadStore(flagVal)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Load()
	if err != nil {
		return nil, nil, err
	}
	return store, st, nil
}

// ---- init ----

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	project := fs.String("project", "", "project name (required, or set MAESTRO_PROJECT)")
	repo := fs.String("repo", "", "absolute path to the git repo (default: detect from cwd)")
	base := fs.String("base", "", "default base branch for new tasks (default: current branch in repo)")
	smokeGate := fs.String("smoke-gate", "", "command(s) to verify a merge (e.g. 'go build ./... && go test ./...')")
	force := fs.Bool("force", false, "overwrite existing project config")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name, err := resolveProject(*project)
	if err != nil {
		return err
	}

	repoPath, err := resolveRepoPath(*repo)
	if err != nil {
		return err
	}
	g := &maestro.Git{RepoPath: repoPath}
	if !g.IsRepo() {
		return fmt.Errorf("%s is not a git working tree", repoPath)
	}
	top, err := g.Toplevel()
	if err != nil {
		return err
	}
	repoPath = top

	baseBranch := *base
	if baseBranch == "" {
		cb, err := g.CurrentBranch()
		if err != nil {
			return fmt.Errorf("detect current branch: %w", err)
		}
		if cb == "HEAD" {
			return errors.New("repo has detached HEAD; pass --base explicitly")
		}
		baseBranch = cb
	}
	if !g.BranchExists(baseBranch) {
		return fmt.Errorf("base branch %q does not exist in %s", baseBranch, repoPath)
	}

	store, err := maestro.NewStore(name)
	if err != nil {
		return err
	}

	if store.Exists() && !*force {
		st, err := store.Load()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "project already initialized; use --force to overwrite")
		return printProject(os.Stdout, &st.Project, *asJSON)
	}

	st := &maestro.State{
		Project: maestro.Project{
			Name:           name,
			RepoPath:       repoPath,
			DefaultBase:    baseBranch,
			SmokeGate:      *smokeGate,
			NextTaskNumber: 1,
		},
	}
	if err := store.Save(st); err != nil {
		return err
	}
	if err := os.MkdirAll(store.WorktreesDir(), 0o755); err != nil {
		return err
	}
	return printProject(os.Stdout, &st.Project, *asJSON)
}

func resolveRepoPath(flagVal string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd, nil
}

// ---- project ----

func cmdProject(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maestro project <list|show|find|update|rename|sweep>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdProjectList(rest)
	case "show":
		return cmdProjectShow(rest)
	case "find":
		return cmdProjectFind(rest)
	case "update":
		return cmdProjectUpdate(rest)
	case "rename":
		return cmdProjectRename(rest)
	case "sweep":
		return cmdProjectSweep(rest)
	default:
		return fmt.Errorf("unknown subcommand: project %s", sub)
	}
}

func cmdProjectList(args []string) error {
	fs := flag.NewFlagSet("project list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	names, err := maestro.ListProjects()
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, names)
	}
	if len(names) == 0 {
		fmt.Println("(no projects)")
		return nil
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return nil
}

func cmdProjectShow(args []string) error {
	fs := flag.NewFlagSet("project show", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	return printProject(os.Stdout, &st.Project, *asJSON)
}

func printProject(w io.Writer, p *maestro.Project, asJSON bool) error {
	if asJSON {
		return writeJSON(w, p)
	}
	fmt.Fprintf(w, "name: %s\n", p.Name)
	fmt.Fprintf(w, "repo_path: %s\n", p.RepoPath)
	fmt.Fprintf(w, "default_base: %s\n", p.DefaultBase)
	if p.SmokeGate != "" {
		fmt.Fprintf(w, "smoke_gate: %s\n", p.SmokeGate)
	}
	fmt.Fprintf(w, "next_task_number: %d\n", p.NextTaskNumber)
	return nil
}

func cmdProjectFind(args []string) error {
	fs := flag.NewFlagSet("project find", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo path to look up (default: cwd)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repoPath, err := resolveRepoPath(*repo)
	if err != nil {
		return err
	}
	matches, err := maestro.FindProjectsByRepo(repoPath)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, matches)
	}
	if len(matches) == 0 {
		fmt.Println("(no match)")
		return nil
	}
	for _, m := range matches {
		fmt.Printf("%s\t%s\n", m.Name, m.Updated.Format(time.RFC3339))
	}
	return nil
}

func cmdProjectUpdate(args []string) error {
	fs := flag.NewFlagSet("project update", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	smokeGate := fs.String("smoke-gate", "", "set smoke gate command(s)")
	clearSmoke := fs.Bool("clear-smoke-gate", false, "clear the smoke gate")
	defaultBase := fs.String("default-base", "", "set the default base branch for new tasks")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	changed := false
	if *clearSmoke {
		st.Project.SmokeGate = ""
		changed = true
	} else if *smokeGate != "" {
		st.Project.SmokeGate = *smokeGate
		changed = true
	}
	if *defaultBase != "" {
		g := &maestro.Git{RepoPath: st.Project.RepoPath}
		if !g.BranchExists(*defaultBase) {
			return fmt.Errorf("branch %q does not exist in %s", *defaultBase, st.Project.RepoPath)
		}
		st.Project.DefaultBase = *defaultBase
		changed = true
	}
	if !changed {
		return errors.New("nothing to update; pass --smoke-gate, --default-base, or --clear-smoke-gate")
	}
	if err := store.Save(st); err != nil {
		return err
	}
	return printProject(os.Stdout, &st.Project, *asJSON)
}

func cmdProjectSweep(args []string) error {
	fs := flag.NewFlagSet("project sweep", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	olderThan := fs.String("older-than", "7d", "tasks last updated longer ago than this are eligible (e.g. 24h, 7d, 30d)")
	statusFilter := fs.String("status", "abandoned", "comma-separated statuses to consider eligible (default: abandoned only - merged tasks are kept as durable history; use `session condense` to summarize them)")
	apply := fs.Bool("apply", false, "actually delete; without --apply this is a dry run")
	keepWT := fs.Bool("keep-worktrees", false, "delete records but leave worktree directories on disk")
	includeMerged := fs.Bool("include-merged", false, "allow `--status` to target merged tasks (destructive: removes durable history)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cutoff, err := parseExtendedDuration(*olderThan)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	statuses := make(map[maestro.TaskStatus]bool)
	for _, s := range strings.Split(*statusFilter, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			statuses[maestro.TaskStatus(s)] = true
		}
	}
	if statuses[maestro.StatusMerged] && !*includeMerged {
		return errors.New("`--status` includes merged but `--include-merged` was not set. Merged tasks are kept as durable history; condense them via `maestro session condense` instead, or pass `--include-merged` to override")
	}
	deadline := time.Now().Add(-cutoff)
	var eligible []*maestro.Task
	for _, t := range st.Tasks {
		if !statuses[t.Status] {
			continue
		}
		if t.UpdatedAt.After(deadline) {
			continue
		}
		eligible = append(eligible, t)
	}

	// Deprecation hint: surface uncondensed merged tasks so the user knows
	// where the durable history lives that this sweep is NOT touching.
	uncondensedMerged := 0
	for _, t := range st.Tasks {
		if t.Status == maestro.StatusMerged && t.CondensedAt.IsZero() {
			uncondensedMerged++
		}
	}

	if *asJSON {
		summaries := make([]map[string]any, 0, len(eligible))
		for _, t := range eligible {
			summaries = append(summaries, map[string]any{
				"id":         t.ID,
				"label":      t.Label,
				"status":     t.Status,
				"updated_at": t.UpdatedAt,
				"worktree":   t.WorktreePath,
			})
		}
		out := map[string]any{
			"dry_run":    !*apply,
			"older_than": *olderThan,
			"eligible":   summaries,
		}
		if err := writeJSON(os.Stdout, out); err != nil {
			return err
		}
	} else {
		if !*apply {
			fmt.Println("dry run; pass --apply to actually delete")
		}
		if uncondensedMerged > 0 && !*includeMerged {
			fmt.Printf("note: %d merged task(s) preserved as durable history; condense via `maestro session condense <id>` instead of deleting\n", uncondensedMerged)
		}
		if len(eligible) == 0 {
			fmt.Println("(nothing to sweep)")
			return nil
		}
		for _, t := range eligible {
			fmt.Printf("%s  %-12s  %s  (updated %s)\n", t.ID, t.Status, taskListLabel(t), t.UpdatedAt.Format(time.RFC3339))
		}
	}

	if !*apply {
		return nil
	}

	g := &maestro.Git{RepoPath: st.Project.RepoPath}
	swept := 0
	for _, t := range eligible {
		if !*keepWT {
			if _, statErr := os.Stat(t.WorktreePath); statErr == nil {
				if err := g.RemoveWorktree(t.WorktreePath, true); err != nil {
					fmt.Fprintf(os.Stderr, "warning: removing worktree for %s: %v\n", t.ID, err)
				}
			}
		}
		st.RemoveTask(t.ID)
		swept++
	}
	if err := store.Save(st); err != nil {
		return err
	}
	if !*asJSON {
		fmt.Printf("swept %d task(s)\n", swept)
	}
	return nil
}

// parseExtendedDuration accepts time.ParseDuration plus a `<N>d` suffix for days,
// since the CLI's natural cutoffs ("7d", "30d") don't fit stdlib duration syntax.
func parseExtendedDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use e.g. 7d, 24h, 90m)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func cmdProjectRename(args []string) error {
	fs := flag.NewFlagSet("project rename", flag.ContinueOnError)
	project := fs.String("project", "", "current project name")
	to := fs.String("to", "", "new project name (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*to) == "" {
		return errors.New("--to is required")
	}
	store, _, err := loadState(*project)
	if err != nil {
		return err
	}
	hasWT, err := store.HasActiveWorktrees()
	if err != nil {
		return err
	}
	if hasWT {
		return errors.New("cannot rename: project has active worktrees. Run `maestro task list --status=active` and clean up first (worktree paths are absolute and would break on rename)")
	}
	newStore, err := store.Rename(*to)
	if err != nil {
		return err
	}
	st, err := newStore.Load()
	if err != nil {
		return err
	}
	return printProject(os.Stdout, &st.Project, *asJSON)
}

// ---- task ----

func cmdTask(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maestro task <new|list|get|get-prompt|update|files|done|abandon|delete|report>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return cmdTaskNew(rest)
	case "list":
		return cmdTaskList(rest)
	case "get":
		return cmdTaskGet(rest)
	case "get-prompt":
		return cmdTaskGetPrompt(rest)
	case "report":
		return cmdTaskReport(rest)
	case "update":
		return cmdTaskUpdate(rest)
	case "files":
		return cmdTaskFiles(rest)
	case "done":
		return cmdTaskDone(rest)
	case "abandon":
		return cmdTaskAbandon(rest)
	case "delete":
		return cmdTaskDelete(rest)
	default:
		return fmt.Errorf("unknown subcommand: task %s", sub)
	}
}

func cmdTaskNew(args []string) error {
	fs := flag.NewFlagSet("task new", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	desc := fs.String("description", "", "task description (required, the short ask)")
	label := fs.String("label", "", "short human-readable label, e.g. 'long press in player' (recommended)")
	base := fs.String("base", "", "base branch (default: project default_base)")
	tags := fs.String("tags", "", "comma-separated tags")
	session := fs.String("session", "", "session ID (defaults to MAESTRO_SESSION env)")
	promptStdin := fs.Bool("prompt-stdin", false, "read full implementer prompt body from stdin")
	promptFile := fs.String("prompt-file", "", "read full implementer prompt body from a file")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*desc) == "" {
		return errors.New("--description is required")
	}
	prompt, err := resolvePromptInput(*promptStdin, *promptFile)
	if err != nil {
		return err
	}
	sessionID := *session
	if sessionID == "" {
		sessionID = os.Getenv("MAESTRO_SESSION")
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}

	baseBranch := *base
	if baseBranch == "" {
		baseBranch = st.Project.DefaultBase
	}
	g := &maestro.Git{RepoPath: st.Project.RepoPath}
	if !g.BranchExists(baseBranch) {
		return fmt.Errorf("base branch %q does not exist in %s", baseBranch, st.Project.RepoPath)
	}
	baseSHA, err := g.ResolveSHA(baseBranch)
	if err != nil {
		return err
	}

	id := st.AllocTaskID()
	branch := "maestro/" + id
	wt := store.WorktreePath(id)
	if err := g.CreateWorktree(wt, branch, baseSHA); err != nil {
		// roll back the ID allocation so we don't leave a hole
		st.Project.NextTaskNumber--
		return fmt.Errorf("create worktree: %w", err)
	}

	now := time.Now()
	t := &maestro.Task{
		ID:                id,
		Label:             strings.TrimSpace(*label),
		Description:       strings.TrimSpace(*desc),
		Status:            maestro.StatusPending,
		Session:           sessionID,
		Branch:            branch,
		BaseBranch:        baseBranch,
		BaseCommit:        baseSHA,
		WorktreePath:      wt,
		ImplementerPrompt: prompt,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if *tags != "" {
		t.AddTags(splitList(*tags))
	}
	st.Tasks = append(st.Tasks, t)
	if err := store.Save(st); err != nil {
		return err
	}
	return printTask(os.Stdout, t, *asJSON)
}

func cmdTaskList(args []string) error {
	fs := flag.NewFlagSet("task list", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	statusFilter := fs.String("status", "", "filter by status (e.g. in_progress,pending). active = all in-flight statuses")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	tasks := st.SortedTasks()
	if *statusFilter != "" {
		tasks = filterByStatus(tasks, *statusFilter)
	}
	if *asJSON {
		return writeJSON(os.Stdout, tasks)
	}
	if len(tasks) == 0 {
		fmt.Println("(no tasks)")
		return nil
	}
	for _, t := range tasks {
		fmt.Printf("%s  %-16s  %s\n", t.ID, t.Status, taskListLabel(t))
	}
	return nil
}

func taskListLabel(t *maestro.Task) string {
	if t.Label != "" {
		return t.Label
	}
	return summarizeOneLine(t.Description)
}

func filterByStatus(tasks []*maestro.Task, filter string) []*maestro.Task {
	parts := strings.Split(filter, ",")
	want := make(map[maestro.TaskStatus]bool, len(parts))
	expandActive := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "active" {
			expandActive = true
			continue
		}
		want[maestro.TaskStatus(p)] = true
	}
	out := make([]*maestro.Task, 0, len(tasks))
	for _, t := range tasks {
		if expandActive && t.Status.IsActive() {
			out = append(out, t)
			continue
		}
		if want[t.Status] {
			out = append(out, t)
		}
	}
	return out
}

func cmdTaskGet(args []string) error {
	fs := flag.NewFlagSet("task get", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	return printTask(os.Stdout, t, *asJSON)
}

// reportHelpHint is appended to every validation error so agents see the
// recovery path without having to guess that --schema exists.
const reportHelpHint = "Run 'maestro task report --schema' to see the full schema and worked examples."

// reportSchemaDoc is the self-documentation an agent gets back from
// 'maestro task report --schema'. Kept here (not in a separate file) so
// the binary stays self-contained. Uses single quotes around code
// references instead of backticks so the Go raw string stays intact.
const reportSchemaDoc = `maestro task report - file a structured JSON report on a task.

USAGE
  maestro task report <task-id> [--source=agent] [--file=path] < report.json
  maestro task report --schema    # print this help

INPUT
  A JSON object read from stdin (or --file). Unknown top-level fields are
  rejected so typos surface as errors instead of silent drops.

REQUIRED FIELDS
  status (string)
    Recommended values:
      Implementer side:  "done" | "needs-info" | "blocked"
      Merge sub-agent:   "merged" | "review-blocked" | "verify-failed" |
                         "smoke-failed" | "conflict-blocked" |
                         "implementer-stale" | "error"
  summary (string)
    2-4 sentence outcome. The PR-description-shaped writeup the
    orchestrator uses to compose the user-facing completion summary.

OPTIONAL FIELDS (implementer side)
  files     ([]string)   files actually modified, paths relative to worktree
  commit    (string)     SHA of the final commit on the task branch
  deferred  ([]string)   items explicitly skipped or out-of-scope, with rationale
  concerns  ([]string)   things to flag to the orchestrator / user
  notes     (string)     free-form additional context

OPTIONAL FIELDS (merge sub-agent side)
  merge_commit     (string)   SHA of the merge commit (when status=merged)
  review_findings  ([]object) see schema below
  verify_notes     (string)   explanation when status=verify-failed
  smoke_tail       (string)   last ~30 lines of failing smoke output

REVIEW FINDING OBJECT
  {
    "severity": "blocking" | "non-blocking",   // required, enum-validated
    "title":    "short one-line summary",      // required
    "details":  "what's wrong and what you'd do instead",  // optional
    "file":     "path/relative/to/repo",       // optional
    "line":     84                              // optional integer
  }

EXAMPLE (implementer)
  maestro task report t14 <<'EOF'
  {
    "status": "done",
    "summary": "rewired credential check to use sync.Once for client dedup.",
    "files": ["auth/login.go", "auth/login_test.go"],
    "commit": "deadbeef12345",
    "deferred": ["token refresh path has the same shape - left for a follow-up"],
    "concerns": ["sync.Once keyed by clientID only; stale credentials could coalesce"]
  }
  EOF

EXAMPLE (merge sub-agent, successful merge with one non-blocking finding)
  maestro task report t14 <<'EOF'
  {
    "status": "merged",
    "summary": "merged after review found one non-blocking concern.",
    "merge_commit": "abc1234567",
    "review_findings": [
      {
        "severity": "non-blocking",
        "title": "consider keying by clientID+credential hash",
        "file": "auth/login.go",
        "line": 84,
        "details": "tightens the dedup contract; not required for correctness"
      }
    ]
  }
  EOF

EXAMPLE (merge sub-agent, smoke gate failed)
  maestro task report t14 <<'EOF'
  {
    "status": "smoke-failed",
    "summary": "tsc failed: TS2304 cannot find name 'qrcode'.",
    "smoke_tail": "...last 30 lines of npx tsc output..."
  }
  EOF

NOTES
  - Side effects: when present, "commit" mirrors to Task.FinalCommit and
    "summary" mirrors to Task.Summary (when previously empty), so
    'maestro status' and 'task list' show useful info without re-parsing
    the latest Note.
  - The report is stored as a Note with type=report. Multiple reports per
    task are kept as a chronological audit trail; the latest is "current."
  - Same schema serves both implementers and merge sub-agents; each fills
    the fields that apply to its role.`

// hasFlag is a loose pre-check for an argv-style boolean flag without
// running the full FlagSet, used so --schema works without requiring a
// positional task ID.
func hasFlag(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// cmdTaskReport reads a JSON Report from stdin (or --file), validates it,
// canonicalizes it, and appends a typed Note (type=report) to the task. This
// is the structured replacement for the legacy `task update --note-content-stdin`
// free-form text reports. Validation catches missing required fields and
// malformed review findings before they land in state; unknown JSON fields
// are rejected so typos surface as errors instead of silent drops.
func cmdTaskReport(args []string) error {
	fs := flag.NewFlagSet("task report", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	source := fs.String("source", "agent", "Note source (agent|orchestrator|user|system)")
	fileFlag := fs.String("file", "", "read JSON from a file instead of stdin")
	schemaFlag := fs.Bool("schema", false, "print the report JSON schema with worked examples, then exit")
	asJSON := fs.Bool("json", false, "JSON output (echo back the stored report)")

	// `--schema` is a discoverability flag for agents: no task ID required.
	// Parse loosely so `maestro task report --schema` works without a
	// positional, but still detect the flag if it appears alongside an ID.
	if hasFlag(args, "--schema") || hasFlag(args, "-schema") {
		_ = fs.Parse(args)
		fmt.Println(reportSchemaDoc)
		_ = schemaFlag // referenced
		return nil
	}

	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}

	var raw []byte
	if *fileFlag != "" {
		raw, err = os.ReadFile(*fileFlag)
		if err != nil {
			return fmt.Errorf("read --file: %w", err)
		}
	} else {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("no report body provided on stdin (or --file). %s", reportHelpHint)
	}

	var report maestro.Report
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return fmt.Errorf("invalid JSON report: %w. %s", err, reportHelpHint)
	}
	if err := report.Validate(); err != nil {
		return fmt.Errorf("report failed validation: %w. %s", err, reportHelpHint)
	}

	canonical, err := json.MarshalIndent(&report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}

	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	t.AddTypedNote(*source, "report", string(canonical))

	// Mirror the report's commit field onto Task.FinalCommit when present,
	// so `task list` / `maestro status` can show the implementer's SHA
	// without having to re-parse the latest Note.
	if report.Commit != "" {
		t.FinalCommit = report.Commit
	}
	if report.Summary != "" && t.Summary == "" {
		t.Summary = report.Summary
	}
	if err := store.Save(st); err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(os.Stdout, &report)
	}
	fmt.Printf("report stored on %s (status=%s, summary=%d chars", id, report.Status, len(report.Summary))
	if len(report.Files) > 0 {
		fmt.Printf(", files=%d", len(report.Files))
	}
	if len(report.Deferred) > 0 {
		fmt.Printf(", deferred=%d", len(report.Deferred))
	}
	if len(report.Concerns) > 0 {
		fmt.Printf(", concerns=%d", len(report.Concerns))
	}
	if len(report.ReviewFindings) > 0 {
		fmt.Printf(", review_findings=%d", len(report.ReviewFindings))
	}
	fmt.Println(")")
	return nil
}

// cmdTaskGetPrompt prints just the ImplementerPrompt for a task. Sub-agents
// run this as their first action to fetch their full task body, so the
// orchestrator never has to inline the prompt twice (once via task new, once
// via the Agent tool prompt).
func cmdTaskGetPrompt(args []string) error {
	fs := flag.NewFlagSet("task get-prompt", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	if t.ImplementerPrompt == "" {
		return fmt.Errorf("task %s has no stored implementer prompt (was it created without --prompt-stdin/--prompt-file?)", id)
	}
	fmt.Print(t.ImplementerPrompt)
	if !strings.HasSuffix(t.ImplementerPrompt, "\n") {
		fmt.Println()
	}
	return nil
}

func cmdTaskUpdate(args []string) error {
	fs := flag.NewFlagSet("task update", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	status := fs.String("status", "", "new status")
	agentID := fs.String("agent-id", "", "agent ID for SendMessage routing")
	note := fs.String("note", "", "append a one-line note (no Type)")
	noteSrc := fs.String("note-source", "orchestrator", "note source label (orchestrator|agent|user|system)")
	noteType := fs.String("note-type", "", "note Type (report|exchange|fold|decision|review|system); used with --note or --note-content-stdin")
	noteStdin := fs.Bool("note-content-stdin", false, "read note content from stdin (use with --note-type and --note-source for typed log entries)")
	addTags := fs.String("add-tags", "", "comma-separated tags to add")
	removeTags := fs.String("remove-tags", "", "comma-separated tags to remove")
	label := fs.String("label", "", "short human-readable label")
	summary := fs.String("summary", "", "update task summary")
	commit := fs.String("commit", "", "update final commit SHA")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	if *status != "" {
		t.Status = maestro.TaskStatus(*status)
		t.UpdatedAt = time.Now()
	}
	if *agentID != "" {
		t.AgentID = *agentID
		t.UpdatedAt = time.Now()
	}
	if *label != "" {
		t.Label = strings.TrimSpace(*label)
		t.UpdatedAt = time.Now()
	}
	if *summary != "" {
		t.Summary = *summary
		t.UpdatedAt = time.Now()
	}
	if *commit != "" {
		t.FinalCommit = *commit
		t.UpdatedAt = time.Now()
	}
	if *note != "" {
		t.AddTypedNote(*noteSrc, *noteType, *note)
	}
	if *noteStdin {
		body, err := readStdin()
		if err != nil {
			return err
		}
		if strings.TrimSpace(body) == "" {
			return errors.New("--note-content-stdin: stdin was empty")
		}
		t.AddTypedNote(*noteSrc, *noteType, body)
	}
	if *addTags != "" {
		t.AddTags(splitList(*addTags))
	}
	if *removeTags != "" {
		t.RemoveTags(splitList(*removeTags))
	}
	if err := store.Save(st); err != nil {
		return err
	}
	return printTask(os.Stdout, t, *asJSON)
}

func cmdTaskFiles(args []string) error {
	fs := flag.NewFlagSet("task files", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	add := fs.String("add", "", "comma-separated files to add")
	remove := fs.String("remove", "", "comma-separated files to remove")
	set := fs.String("set", "", "comma-separated files to replace the list with")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	if *set != "" {
		t.DeclaredFiles = nil
		t.MergeFiles(splitList(*set))
	}
	if *add != "" {
		t.MergeFiles(splitList(*add))
	}
	if *remove != "" {
		t.RemoveFiles(splitList(*remove))
	}
	if err := store.Save(st); err != nil {
		return err
	}
	return printTask(os.Stdout, t, *asJSON)
}

func cmdTaskDone(args []string) error {
	fs := flag.NewFlagSet("task done", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	summary := fs.String("summary", "", "summary of what was done")
	commit := fs.String("commit", "", "final commit SHA on the task branch")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = maestro.StatusMerged
	if *summary != "" {
		t.Summary = *summary
	}
	if *commit != "" {
		t.FinalCommit = *commit
	}
	t.UpdatedAt = time.Now()
	if err := store.Save(st); err != nil {
		return err
	}
	return printTask(os.Stdout, t, *asJSON)
}

func cmdTaskAbandon(args []string) error {
	fs := flag.NewFlagSet("task abandon", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	note := fs.String("note", "", "reason for abandonment (recommended)")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = maestro.StatusAbandoned
	t.UpdatedAt = time.Now()
	if *note != "" {
		t.AddNote("orchestrator", *note)
	}
	if err := store.Save(st); err != nil {
		return err
	}
	return printTask(os.Stdout, t, *asJSON)
}

func cmdTaskDelete(args []string) error {
	fs := flag.NewFlagSet("task delete", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	keepWT := fs.Bool("keep-worktree", false, "remove the task record but leave the worktree on disk")
	force := fs.Bool("force", false, "delete even if the task is still active")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	if t.Status.IsActive() && !*force {
		return fmt.Errorf("task %s is %s; pass --force to delete an active task", id, t.Status)
	}
	wtRemoved := false
	if !*keepWT {
		if _, statErr := os.Stat(t.WorktreePath); statErr == nil {
			g := &maestro.Git{RepoPath: st.Project.RepoPath}
			if err := g.RemoveWorktree(t.WorktreePath, true); err != nil {
				fmt.Fprintf(os.Stderr, "warning: removing worktree at %s: %v\n", t.WorktreePath, err)
			} else {
				wtRemoved = true
			}
		}
	}
	st.RemoveTask(id)
	if err := store.Save(st); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, map[string]any{
			"deleted":         id,
			"worktree_removed": wtRemoved,
			"kept_worktree":   *keepWT,
		})
	}
	fmt.Printf("deleted: %s", id)
	if wtRemoved {
		fmt.Printf(" (worktree removed)")
	} else if *keepWT {
		fmt.Printf(" (worktree kept at %s)", t.WorktreePath)
	}
	fmt.Println()
	return nil
}

// ---- conflicts ----

func cmdConflicts(args []string) error {
	fs := flag.NewFlagSet("conflicts", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	files := fs.String("files", "", "additional files to check (comma-separated). Combined with the task's declared files.")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	check := append([]string{}, t.DeclaredFiles...)
	if *files != "" {
		check = append(check, splitList(*files)...)
	}
	conflicts := st.ConflictingTasks(id, check)
	if *asJSON {
		out := make([]map[string]any, 0, len(conflicts))
		for _, c := range conflicts {
			out = append(out, map[string]any{
				"id":             c.ID,
				"status":         c.Status,
				"description":    c.Description,
				"declared_files": c.DeclaredFiles,
			})
		}
		return writeJSON(os.Stdout, out)
	}
	if len(conflicts) == 0 {
		fmt.Println("(no conflicts)")
		return nil
	}
	for _, c := range conflicts {
		fmt.Printf("%s  %-16s  %s\n", c.ID, c.Status, taskListLabel(c))
		for _, f := range overlapFiles(check, c.DeclaredFiles) {
			fmt.Printf("    %s\n", f)
		}
	}
	return nil
}

func overlapFiles(a, b []string) []string {
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	var out []string
	for _, x := range b {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

// ---- worktree ----

func cmdWorktree(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maestro worktree <path|cleanup|restore>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "path":
		return cmdWorktreePath(rest)
	case "cleanup":
		return cmdWorktreeCleanup(rest)
	case "restore":
		return cmdWorktreeRestore(rest)
	default:
		return fmt.Errorf("unknown subcommand: worktree %s", sub)
	}
}

func cmdWorktreePath(args []string) error {
	fs := flag.NewFlagSet("worktree path", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	fmt.Println(t.WorktreePath)
	return nil
}

func cmdWorktreeCleanup(args []string) error {
	fs := flag.NewFlagSet("worktree cleanup", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	force := fs.Bool("force", false, "force-remove even with uncommitted changes")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	g := &maestro.Git{RepoPath: st.Project.RepoPath}
	if err := g.RemoveWorktree(t.WorktreePath, *force); err != nil {
		return err
	}
	fmt.Printf("removed: %s\n", t.WorktreePath)
	return nil
}

func cmdWorktreeRestore(args []string) error {
	fs := flag.NewFlagSet("worktree restore", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	t := st.FindTask(id)
	if t == nil {
		return fmt.Errorf("task %s not found", id)
	}
	if _, err := os.Stat(t.WorktreePath); err == nil {
		return fmt.Errorf("worktree already exists at %s", t.WorktreePath)
	}
	g := &maestro.Git{RepoPath: st.Project.RepoPath}
	if !g.BranchExists(t.Branch) {
		return fmt.Errorf("branch %s no longer exists; restore is only supported for tasks whose branch is intact (merged tasks have their branch deleted)", t.Branch)
	}
	if err := g.AttachWorktree(t.WorktreePath, t.Branch); err != nil {
		return err
	}
	fmt.Printf("restored: %s (branch %s)\n", t.WorktreePath, t.Branch)
	return nil
}

// ---- web ----

// cmdWeb starts a local browser UI for exploring maestro state. Read-only.
func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	port := fs.Int("port", 9876, "TCP port to listen on")
	bind := fs.String("bind", "127.0.0.1", "interface to bind (default localhost only)")
	openBrowser := fs.Bool("open", true, "open the default browser when ready")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", *bind, *port)
	return web.Serve(addr, *openBrowser)
}

// ---- statusline ----

// cmdStatusline emits one line summarizing active tasks, suitable for
// configuring as a Claude Code statusLine. Designed to fail silently
// (empty output) so it never injects errors into the agent's display.
//
// Project resolution: --project flag, then MAESTRO_PROJECT env, then
// auto-detect from cwd via FindProjectsByRepo. Most-recently-updated wins
// when multiple maestro projects target the same repo.
func cmdStatusline(args []string) error {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	project := fs.String("project", "", "project name (defaults to MAESTRO_PROJECT or cwd auto-detect)")
	omitName := fs.Bool("no-project-name", false, "don't prefix output with the project name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := resolveStatuslineProject(*project)
	if name == "" {
		// No project to report on; print nothing so the statusline stays clean.
		return nil
	}
	store, err := maestro.NewStore(name)
	if err != nil {
		return nil
	}
	st, err := store.Load()
	if err != nil {
		return nil
	}

	var pending, inProgress, awaiting, blocked int
	for _, t := range st.Tasks {
		switch t.Status {
		case maestro.StatusPending:
			pending++
		case maestro.StatusInProgress:
			inProgress++
		case maestro.StatusAwaitingReview:
			awaiting++
		case maestro.StatusBlocked:
			blocked++
		}
	}

	var parts []string
	if inProgress > 0 {
		parts = append(parts, fmt.Sprintf("%d in-progress", inProgress))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	if awaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting", awaiting))
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}

	body := "no active tasks"
	if len(parts) > 0 {
		body = strings.Join(parts, " · ")
	}
	if *omitName {
		fmt.Println(body)
	} else {
		fmt.Printf("%s: %s\n", name, body)
	}
	return nil
}

// cmdStatus prints a multi-line snapshot of the project: active tasks
// (sorted by status priority) and the last few merges. Designed to be
// orchestrator-friendly - tight format, no narrative needed.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	project := fs.String("project", "", "project name (defaults to MAESTRO_PROJECT or cwd auto-detect)")
	lastMerged := fs.Int("last-merged", 3, "how many recently merged tasks to show (0 to omit)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := resolveStatuslineProject(*project)
	if name == "" {
		return errors.New("no project specified and none auto-detected (pass --project or set MAESTRO_PROJECT)")
	}
	store, err := maestro.NewStore(name)
	if err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}

	var active, merged []*maestro.Task
	for _, t := range st.Tasks {
		if t.Status.IsActive() {
			active = append(active, t)
		} else if t.Status == maestro.StatusMerged {
			merged = append(merged, t)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		oi, oj := statusOrder(active[i].Status), statusOrder(active[j].Status)
		if oi != oj {
			return oi < oj
		}
		return active[i].UpdatedAt.Before(active[j].UpdatedAt)
	})
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].UpdatedAt.After(merged[j].UpdatedAt)
	})
	if len(merged) > *lastMerged {
		merged = merged[:*lastMerged]
	}

	if *asJSON {
		out := map[string]any{
			"project": name,
			"active":  taskSummariesForStatus(active),
		}
		if *lastMerged > 0 {
			out["recently_merged"] = taskSummariesForStatus(merged)
		}
		return writeJSON(os.Stdout, out)
	}

	fmt.Println(name)
	fmt.Println()
	if len(active) == 0 {
		fmt.Println("(no active tasks)")
	} else {
		for _, t := range active {
			fmt.Printf("  %-4s %-16s %-50s (%s)\n", t.ID, t.Status, taskListLabel(t), humanizeAge(time.Since(t.UpdatedAt)))
		}
	}
	if *lastMerged > 0 && len(merged) > 0 {
		fmt.Println()
		fmt.Printf("Recently merged (last %d):\n", len(merged))
		for _, t := range merged {
			fmt.Printf("  %-4s %-50s (%s ago)\n", t.ID, taskListLabel(t), humanizeAge(time.Since(t.UpdatedAt)))
		}
	}
	return nil
}

func taskSummariesForStatus(tasks []*maestro.Task) []map[string]any {
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, map[string]any{
			"id":         t.ID,
			"label":      t.Label,
			"status":     t.Status,
			"updated_at": t.UpdatedAt,
			"age":        humanizeAge(time.Since(t.UpdatedAt)),
		})
	}
	return out
}

// statusOrder gives a deterministic ranking for active task statuses so
// the most-actionable rows surface first in `status` output.
func statusOrder(s maestro.TaskStatus) int {
	switch s {
	case maestro.StatusInProgress:
		return 0
	case maestro.StatusAwaitingReview:
		return 1
	case maestro.StatusBlocked:
		return 2
	case maestro.StatusPending:
		return 3
	}
	return 9
}

// humanizeAge renders a duration as "12s", "3m", "2h15m", "4d". Designed
// to fit in a terminal column without truncation.
func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// resolveStatuslineProject is the lenient version of resolveProject.
// Returns "" instead of an error so the statusline silently produces no
// output when there's nothing to report.
func resolveStatuslineProject(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("MAESTRO_PROJECT"); env != "" {
		return env
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	g := &maestro.Git{RepoPath: cwd}
	if !g.IsRepo() {
		return ""
	}
	top, err := g.Toplevel()
	if err != nil {
		return ""
	}
	matches, err := maestro.FindProjectsByRepo(top)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0].Name
}

// ---- session ----

func cmdSession(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maestro session <start|list|get|current|pending-condense|condense>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return cmdSessionStart(rest)
	case "list":
		return cmdSessionList(rest)
	case "get":
		return cmdSessionGet(rest)
	case "current":
		return cmdSessionCurrent(rest)
	case "pending-condense":
		return cmdSessionPendingCondense(rest)
	case "condense":
		return cmdSessionCondense(rest)
	default:
		return fmt.Errorf("unknown subcommand: session %s", sub)
	}
}

func cmdSessionStart(args []string) error {
	fs := flag.NewFlagSet("session start", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	name := fs.String("name", "", "human-readable session name (recommended)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	id := st.AllocSessionID()
	s := &maestro.Session{
		ID:        id,
		Name:      strings.TrimSpace(*name),
		StartedAt: time.Now(),
	}
	st.Sessions = append(st.Sessions, s)
	if err := store.Save(st); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, s)
	}
	printSession(os.Stdout, s, false)
	fmt.Println()
	fmt.Printf("export MAESTRO_SESSION=%s\n", id)
	return nil
}

func cmdSessionList(args []string) error {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	includeCondensed := fs.Bool("include-condensed", true, "include sessions that have been condensed (default true)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	out := make([]*maestro.Session, 0, len(st.Sessions))
	for _, s := range st.Sessions {
		if !*includeCondensed && !s.EndedAt.IsZero() {
			continue
		}
		out = append(out, s)
	}
	if *asJSON {
		return writeJSON(os.Stdout, out)
	}
	if len(out) == 0 {
		fmt.Println("(no sessions)")
		return nil
	}
	for _, s := range out {
		state := "active"
		if !s.EndedAt.IsZero() {
			state = "condensed"
		}
		name := s.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Printf("%s  %-9s  %s  (started %s)\n", s.ID, state, name, s.StartedAt.Format(time.RFC3339))
	}
	return nil
}

func cmdSessionGet(args []string) error {
	fs := flag.NewFlagSet("session get", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	s := st.FindSession(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	tasks := st.TasksInSession(id)
	if *asJSON {
		return writeJSON(os.Stdout, map[string]any{
			"session": s,
			"tasks":   tasks,
		})
	}
	printSession(os.Stdout, s, true)
	fmt.Println()
	if len(tasks) == 0 {
		fmt.Println("(no tasks in this session)")
		return nil
	}
	fmt.Printf("Tasks (%d):\n", len(tasks))
	for _, t := range tasks {
		fmt.Printf("  %-4s %-16s %s\n", t.ID, t.Status, taskListLabel(t))
	}
	return nil
}

func cmdSessionCurrent(args []string) error {
	fs := flag.NewFlagSet("session current", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cur := os.Getenv("MAESTRO_SESSION")
	if cur == "" {
		fmt.Println("(no current session)")
		return nil
	}
	fmt.Println(cur)
	return nil
}

// cmdSessionPendingCondense dumps the data the orchestrator needs to write a
// condensed summary: each task's label, summary, tags, file list, and any
// notes typed `report` or `decision`. The orchestrator reads this output,
// composes a session-level summary, and feeds it back via session condense
// --apply --summary-stdin.
func cmdSessionPendingCondense(args []string) error {
	fs := flag.NewFlagSet("session pending-condense", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	s := st.FindSession(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	if !s.EndedAt.IsZero() {
		return fmt.Errorf("session %s is already condensed", id)
	}
	tasks := st.TasksInSession(id)
	type filteredTask struct {
		ID          string         `json:"id"`
		Label       string         `json:"label,omitempty"`
		Description string         `json:"description"`
		Status      string         `json:"status"`
		Tags        []string       `json:"tags,omitempty"`
		Summary     string         `json:"summary,omitempty"`
		Files       []string       `json:"files,omitempty"`
		FinalCommit string         `json:"final_commit,omitempty"`
		KeyNotes    []maestro.Note `json:"key_notes,omitempty"`
	}
	out := make([]filteredTask, 0, len(tasks))
	for _, t := range tasks {
		ft := filteredTask{
			ID:          t.ID,
			Label:       t.Label,
			Description: t.Description,
			Status:      string(t.Status),
			Tags:        t.Tags,
			Summary:     t.Summary,
			Files:       t.DeclaredFiles,
			FinalCommit: t.FinalCommit,
		}
		for _, n := range t.Notes {
			if n.Type == "report" || n.Type == "decision" || n.Type == "review" {
				ft.KeyNotes = append(ft.KeyNotes, n)
			}
		}
		out = append(out, ft)
	}
	if *asJSON {
		return writeJSON(os.Stdout, map[string]any{
			"session": s,
			"tasks":   out,
		})
	}
	// Default human output is markdown-ish so the orchestrator can copy
	// straight into its summarization prompt.
	fmt.Printf("# Session: %s", s.ID)
	if s.Name != "" {
		fmt.Printf(" (%s)", s.Name)
	}
	fmt.Println()
	fmt.Printf("Started: %s\n", s.StartedAt.Format(time.RFC3339))
	fmt.Println()
	for _, t := range out {
		fmt.Printf("## %s: %s\n", t.ID, t.Label)
		fmt.Printf("Status: %s\n", t.Status)
		if len(t.Tags) > 0 {
			fmt.Printf("Tags: %s\n", strings.Join(t.Tags, ", "))
		}
		fmt.Printf("Description: %s\n", t.Description)
		if t.Summary != "" {
			fmt.Printf("Summary: %s\n", t.Summary)
		}
		if t.FinalCommit != "" {
			fmt.Printf("Final commit: %s\n", t.FinalCommit)
		}
		if len(t.Files) > 0 {
			fmt.Printf("Files: %s\n", strings.Join(t.Files, ", "))
		}
		for _, n := range t.KeyNotes {
			fmt.Printf("- [%s/%s] %s\n", n.Source, n.Type, summarizeOneLine(n.Content))
		}
		fmt.Println()
	}
	return nil
}

func cmdSessionCondense(args []string) error {
	fs := flag.NewFlagSet("session condense", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	summaryStdin := fs.Bool("summary-stdin", false, "read condensed summary from stdin")
	summaryFile := fs.String("summary-file", "", "read condensed summary from a file")
	apply := fs.Bool("apply", false, "actually condense; without this flag the command is a dry run")
	asJSON := fs.Bool("json", false, "JSON output")
	id, err := parseFlagsWithID(fs, args)
	if err != nil {
		return err
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	s := st.FindSession(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	if !s.EndedAt.IsZero() {
		return fmt.Errorf("session %s is already condensed", id)
	}
	summary, err := resolvePromptInput(*summaryStdin, *summaryFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(summary) == "" {
		return errors.New("session condense needs --summary-stdin or --summary-file with non-empty content")
	}
	tasks := st.TasksInSession(id)

	if *asJSON {
		preview := map[string]any{
			"dry_run":  !*apply,
			"session":  s,
			"summary":  summary,
			"affected": len(tasks),
		}
		if err := writeJSON(os.Stdout, preview); err != nil {
			return err
		}
	} else {
		if !*apply {
			fmt.Println("dry run; pass --apply to commit the condensation")
		}
		fmt.Printf("Session %s would be marked condensed.\n", id)
		fmt.Printf("Summary length: %d chars.\n", len(summary))
		fmt.Printf("Tasks affected: %d.\n", len(tasks))
		for _, t := range tasks {
			fmt.Printf("  %s  %s  -> trim ImplementerPrompt + verbose Notes\n", t.ID, taskListLabel(t))
		}
	}
	if !*apply {
		return nil
	}

	s.Condensed = summary
	s.EndedAt = time.Now()
	for _, t := range tasks {
		t.Condense()
	}
	if err := store.Save(st); err != nil {
		return err
	}
	if !*asJSON {
		fmt.Printf("condensed %d task(s) in session %s\n", len(tasks), id)
	}
	return nil
}

func printSession(w io.Writer, s *maestro.Session, includeBody bool) {
	fmt.Fprintf(w, "id: %s\n", s.ID)
	if s.Name != "" {
		fmt.Fprintf(w, "name: %s\n", s.Name)
	}
	fmt.Fprintf(w, "started_at: %s\n", s.StartedAt.Format(time.RFC3339))
	if !s.EndedAt.IsZero() {
		fmt.Fprintf(w, "ended_at: %s\n", s.EndedAt.Format(time.RFC3339))
		if s.Condensed != "" && includeBody {
			fmt.Fprintln(w, "condensed:")
			fmt.Fprintln(w, s.Condensed)
		}
	}
}

// ---- tag ----

func cmdTag(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maestro tag <list|rename>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdTagList(rest)
	case "rename":
		return cmdTagRename(rest)
	default:
		return fmt.Errorf("unknown subcommand: tag %s", sub)
	}
}

func cmdTagList(args []string) error {
	fs := flag.NewFlagSet("tag list", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	withCounts := fs.Bool("with-counts", false, "include task counts per tag")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}
	counts := st.AllTags()
	tags := make([]string, 0, len(counts))
	for tag := range counts {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	if *asJSON {
		out := make([]map[string]any, 0, len(tags))
		for _, tag := range tags {
			out = append(out, map[string]any{"tag": tag, "count": counts[tag]})
		}
		return writeJSON(os.Stdout, out)
	}
	if len(tags) == 0 {
		fmt.Println("(no tags)")
		return nil
	}
	for _, tag := range tags {
		if *withCounts {
			fmt.Printf("%-30s %d\n", tag, counts[tag])
		} else {
			fmt.Println(tag)
		}
	}
	return nil
}

func cmdTagRename(args []string) error {
	fs := flag.NewFlagSet("tag rename", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	from := fs.String("from", "", "old tag name (required)")
	to := fs.String("to", "", "new tag name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("--from and --to are required")
	}
	store, st, err := loadState(*project)
	if err != nil {
		return err
	}
	changed := st.RenameTagAcrossTasks(*from, *to)
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Printf("renamed %q -> %q on %d task(s)\n", *from, *to, changed)
	return nil
}

// ---- search ----

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	text := fs.String("text", "", "case-insensitive substring match against label/description/summary")
	tags := fs.String("tag", "", "comma-separated tags (any-of match)")
	session := fs.String("session", "", "exact session ID")
	statusFilter := fs.String("status", "", "comma-separated statuses (e.g. merged,abandoned)")
	since := fs.String("since", "", "ISO timestamp; include only tasks updated >= this")
	until := fs.String("until", "", "ISO timestamp; include only tasks updated <= this")
	limit := fs.Int("limit", 20, "max results (0 for unlimited)")
	full := fs.Bool("full", false, "JSON output: include verbose Notes (default omits them)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := loadState(*project)
	if err != nil {
		return err
	}

	q := maestro.SearchQuery{
		Text:    *text,
		Session: *session,
		Limit:   *limit,
	}
	if *tags != "" {
		q.Tags = splitList(*tags)
	}
	if *statusFilter != "" {
		for _, s := range splitList(*statusFilter) {
			q.Statuses = append(q.Statuses, maestro.TaskStatus(s))
		}
	}
	if *since != "" {
		t, err := parseSearchTime(*since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		q.Since = t
	}
	if *until != "" {
		t, err := parseSearchTime(*until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		q.Until = t
	}

	results := st.SearchTasks(q)

	if *asJSON {
		if *full {
			return writeJSON(os.Stdout, results)
		}
		// Default: drop verbose fields
		out := make([]map[string]any, 0, len(results))
		for _, t := range results {
			out = append(out, map[string]any{
				"id":           t.ID,
				"label":        t.Label,
				"status":       t.Status,
				"session":      t.Session,
				"tags":         t.Tags,
				"summary":      t.Summary,
				"final_commit": t.FinalCommit,
				"updated_at":   t.UpdatedAt,
			})
		}
		return writeJSON(os.Stdout, out)
	}

	if len(results) == 0 {
		fmt.Println("(no matches)")
		return nil
	}
	for _, t := range results {
		ses := t.Session
		if ses == "" {
			ses = "-"
		}
		tags := strings.Join(t.Tags, ",")
		if tags == "" {
			tags = "-"
		}
		fmt.Printf("%-4s %-16s %-12s %-30s %s\n", t.ID, t.Status, ses, tags, taskListLabel(t))
	}
	return nil
}

// parseSearchTime accepts RFC3339 or yyyy-mm-dd.
func parseSearchTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC3339 or yyyy-mm-dd)", s)
}

// ---- helpers ----

// resolvePromptInput reads the implementer prompt body from stdin, a file,
// or returns "" if neither was specified. Mutual exclusion is enforced.
func resolvePromptInput(stdinFlag bool, file string) (string, error) {
	if stdinFlag && file != "" {
		return "", errors.New("--prompt-stdin and --prompt-file are mutually exclusive")
	}
	if stdinFlag {
		return readStdin()
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read prompt file: %w", err)
		}
		return string(data), nil
	}
	return "", nil
}

// readStdin slurps stdin to EOF. Used by --prompt-stdin and
// --note-content-stdin. Heredoc-friendly.
func readStdin() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(data), nil
}

// parseFlagsWithID parses flags allowing the task ID to appear in any
// position (before flags, after flags, or between flags). The stdlib flag
// package stops at the first non-flag arg, so we re-parse remaining args
// after pulling the positional out.
func parseFlagsWithID(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", errors.New("missing task ID")
	}
	id := rest[0]
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return "", err
		}
		if leftover := fs.Args(); len(leftover) > 0 {
			return "", fmt.Errorf("unexpected extra args: %s", strings.Join(leftover, " "))
		}
	}
	return id, nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func summarizeOneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printTask(w io.Writer, t *maestro.Task, asJSON bool) error {
	if asJSON {
		return writeJSON(w, t)
	}
	fmt.Fprintf(w, "id: %s\n", t.ID)
	if t.Label != "" {
		fmt.Fprintf(w, "label: %s\n", t.Label)
	}
	fmt.Fprintf(w, "status: %s\n", t.Status)
	if t.Session != "" {
		fmt.Fprintf(w, "session: %s\n", t.Session)
	}
	if len(t.Tags) > 0 {
		fmt.Fprintf(w, "tags: %s\n", strings.Join(t.Tags, ","))
	}
	fmt.Fprintf(w, "description: %s\n", summarizeOneLine(t.Description))
	fmt.Fprintf(w, "branch: %s\n", t.Branch)
	fmt.Fprintf(w, "base_branch: %s\n", t.BaseBranch)
	fmt.Fprintf(w, "base_commit: %s\n", t.BaseCommit)
	fmt.Fprintf(w, "worktree: %s\n", t.WorktreePath)
	if t.AgentID != "" {
		fmt.Fprintf(w, "agent_id: %s\n", t.AgentID)
	}
	if t.ImplementerPrompt != "" {
		fmt.Fprintf(w, "implementer_prompt: %d chars (use `task get-prompt %s` to read)\n", len(t.ImplementerPrompt), t.ID)
	}
	if len(t.DeclaredFiles) > 0 {
		fmt.Fprintf(w, "declared_files: %s\n", strings.Join(t.DeclaredFiles, ","))
	}
	if t.FinalCommit != "" {
		fmt.Fprintf(w, "final_commit: %s\n", t.FinalCommit)
	}
	if t.Summary != "" {
		fmt.Fprintf(w, "summary: %s\n", summarizeOneLine(t.Summary))
	}
	if len(t.Notes) > 0 {
		fmt.Fprintf(w, "notes: %d\n", len(t.Notes))
	}
	if !t.CondensedAt.IsZero() {
		fmt.Fprintf(w, "condensed_at: %s\n", t.CondensedAt.Format(time.RFC3339))
	}
	return nil
}
