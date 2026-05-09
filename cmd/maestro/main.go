// maestro is a small CLI helper for the maestro orchestration skill. It
// tracks tasks and worktrees for a single project and gives the orchestrator
// agent a deterministic substrate so it doesn't have to reinvent state
// management every session.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fisherevans/maestro/internal/maestro"
)

const usage = `maestro - state and worktree helper for the maestro orchestration skill

Usage:
  maestro <command> [flags]

Project commands:
  init                       Initialize a project (creates ~/.maestro/<project>/)
  project list               List all initialized projects
  project show               Show current project config

Task commands:
  task new                   Create a task and worktree
  task list                  List tasks for a project
  task get <id>              Show one task
  task update <id>           Update task fields
  task files <id>            Manage declared file list
  task done <id>             Mark a task merged
  task abandon <id>          Mark a task abandoned

Coordination:
  conflicts <id>             Show declared-file overlap with other active tasks
  worktree path <id>         Print absolute path of a task's worktree
  worktree cleanup <id>      Remove a task's worktree

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
		return errors.New("usage: maestro project <list|show>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdProjectList(rest)
	case "show":
		return cmdProjectShow(rest)
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
	fmt.Fprintf(w, "next_task_number: %d\n", p.NextTaskNumber)
	return nil
}

// ---- task ----

func cmdTask(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: maestro task <new|list|get|update|files|done|abandon>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return cmdTaskNew(rest)
	case "list":
		return cmdTaskList(rest)
	case "get":
		return cmdTaskGet(rest)
	case "update":
		return cmdTaskUpdate(rest)
	case "files":
		return cmdTaskFiles(rest)
	case "done":
		return cmdTaskDone(rest)
	case "abandon":
		return cmdTaskAbandon(rest)
	default:
		return fmt.Errorf("unknown subcommand: task %s", sub)
	}
}

func cmdTaskNew(args []string) error {
	fs := flag.NewFlagSet("task new", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	desc := fs.String("description", "", "task description (required)")
	base := fs.String("base", "", "base branch (default: project default_base)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*desc) == "" {
		return errors.New("--description is required")
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
		ID:           id,
		Description:  strings.TrimSpace(*desc),
		Status:       maestro.StatusPending,
		Branch:       branch,
		BaseBranch:   baseBranch,
		BaseCommit:   baseSHA,
		WorktreePath: wt,
		CreatedAt:    now,
		UpdatedAt:    now,
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
		fmt.Printf("%s  %-16s  %s\n", t.ID, t.Status, summarizeOneLine(t.Description))
	}
	return nil
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

func cmdTaskUpdate(args []string) error {
	fs := flag.NewFlagSet("task update", flag.ContinueOnError)
	project := fs.String("project", "", "project name")
	status := fs.String("status", "", "new status")
	agentID := fs.String("agent-id", "", "agent ID for SendMessage routing")
	note := fs.String("note", "", "append a note (audit trail)")
	noteSrc := fs.String("note-source", "orchestrator", "note source label (orchestrator|agent|user)")
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
	if *summary != "" {
		t.Summary = *summary
		t.UpdatedAt = time.Now()
	}
	if *commit != "" {
		t.FinalCommit = *commit
		t.UpdatedAt = time.Now()
	}
	if *note != "" {
		t.AddNote(*noteSrc, *note)
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
		fmt.Printf("%s  %-16s  %s\n", c.ID, c.Status, summarizeOneLine(c.Description))
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
		return errors.New("usage: maestro worktree <path|cleanup>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "path":
		return cmdWorktreePath(rest)
	case "cleanup":
		return cmdWorktreeCleanup(rest)
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

// ---- helpers ----

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
	fmt.Fprintf(w, "status: %s\n", t.Status)
	fmt.Fprintf(w, "description: %s\n", summarizeOneLine(t.Description))
	fmt.Fprintf(w, "branch: %s\n", t.Branch)
	fmt.Fprintf(w, "base_branch: %s\n", t.BaseBranch)
	fmt.Fprintf(w, "base_commit: %s\n", t.BaseCommit)
	fmt.Fprintf(w, "worktree: %s\n", t.WorktreePath)
	if t.AgentID != "" {
		fmt.Fprintf(w, "agent_id: %s\n", t.AgentID)
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
	return nil
}
