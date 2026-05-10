// Package maestro implements state and worktree management for the maestro
// orchestration skill. The CLI in cmd/maestro is a thin shell over this package.
//
// State lives at ~/.maestro/<project>/state.json. Worktrees live at
// ~/.maestro/<project>/wt/<task-id>/. Each project is fully self-contained;
// nothing is shared across projects.
package maestro

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const stateFileName = "state.json"

// TaskStatus is the lifecycle state of a single task.
type TaskStatus string

const (
	StatusPending        TaskStatus = "pending"
	StatusInProgress     TaskStatus = "in_progress"
	StatusAwaitingReview TaskStatus = "awaiting_review"
	StatusMerged         TaskStatus = "merged"
	StatusBlocked        TaskStatus = "blocked"
	StatusAbandoned      TaskStatus = "abandoned"
)

// IsActive reports whether a task is in flight (i.e. should be considered
// when checking for file conflicts or counting toward the parallelism cap).
func (s TaskStatus) IsActive() bool {
	switch s {
	case StatusPending, StatusInProgress, StatusAwaitingReview, StatusBlocked:
		return true
	}
	return false
}

// State is the entire on-disk state for a single maestro project.
type State struct {
	Project  Project    `json:"project"`
	Tasks    []*Task    `json:"tasks"`
	Sessions []*Session `json:"sessions,omitempty"`
	Updated  time.Time  `json:"updated"`
}

// Project holds the config for a maestro project: which repo it tracks,
// what branch new tasks default to, and the smoke gate to run after merges.
type Project struct {
	Name              string `json:"name"`
	RepoPath          string `json:"repo_path"`
	DefaultBase       string `json:"default_base"`
	SmokeGate         string `json:"smoke_gate,omitempty"`
	NextTaskNumber    int    `json:"next_task_number"`
	NextSessionNumber int    `json:"next_session_number,omitempty"`
}

// Session groups a set of tasks worked on together. Multiple sessions can
// run concurrently against the same project (different shells, different
// agents). When the orchestrator transitions focus or hits a milestone, it
// proposes a `session condense` which compresses the session's verbose log
// into Condensed and trims the underlying tasks.
type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Condensed string    `json:"condensed,omitempty"`
}

// Task is one unit of work assigned to a sub-agent. Tasks are durable: they
// outlive the session that created them and serve as a queryable record of
// what was asked, what was done, and why. Verbose fields (ImplementerPrompt,
// Notes) are trimmed by `session condense` once the orchestrator has
// summarized the session into Session.Condensed.
type Task struct {
	ID                string     `json:"id"`
	Label             string     `json:"label,omitempty"`
	Description       string     `json:"description"`
	Status            TaskStatus `json:"status"`
	Session           string     `json:"session,omitempty"`
	Tags              []string   `json:"tags,omitempty"`
	Branch            string     `json:"branch"`
	BaseBranch        string     `json:"base_branch"`
	BaseCommit        string     `json:"base_commit"`
	WorktreePath      string     `json:"worktree_path"`
	DeclaredFiles     []string   `json:"declared_files"`
	AgentID           string     `json:"agent_id"`
	ImplementerPrompt string     `json:"implementer_prompt,omitempty"`
	Summary           string     `json:"summary"`
	FinalCommit       string     `json:"final_commit"`
	Notes             []Note     `json:"notes"`
	CondensedAt       time.Time  `json:"condensed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Note is a timestamped log entry on a task. Type classifies the entry so the
// log is filterable: report (a sub-agent's final report), exchange (a back-
// and-forth message), fold (an orchestrator-side refinement injected after
// spawn), decision (a constraint or call-out worth preserving through
// condensation), system (CLI-side bookkeeping). Type is optional for
// backwards compatibility with notes written before this field existed.
type Note struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Type    string    `json:"type,omitempty"`
	Content string    `json:"content"`
}

// Store is bound to a single project name and resolves all paths under
// ~/.maestro/<name>/.
type Store struct {
	Root        string
	ProjectName string
}

// NewStore validates the project name and returns a Store rooted at the
// user's home directory. Use ListProjects on a *Store with empty ProjectName
// for cross-project queries.
func NewStore(projectName string) (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	if projectName != "" {
		if err := validateProjectName(projectName); err != nil {
			return nil, err
		}
	}
	return &Store{
		Root:        filepath.Join(home, ".maestro"),
		ProjectName: projectName,
	}, nil
}

// ProjectDir is ~/.maestro/<project>.
func (s *Store) ProjectDir() string {
	return filepath.Join(s.Root, s.ProjectName)
}

// StateFile is the JSON state file for the current project.
func (s *Store) StateFile() string {
	return filepath.Join(s.ProjectDir(), stateFileName)
}

// WorktreesDir is the parent directory under which per-task worktrees live.
func (s *Store) WorktreesDir() string {
	return filepath.Join(s.ProjectDir(), "wt")
}

// WorktreePath returns the directory for a specific task's worktree.
func (s *Store) WorktreePath(taskID string) string {
	return filepath.Join(s.WorktreesDir(), taskID)
}

// Exists reports whether the project's state file is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.StateFile())
	return err == nil
}

// Load reads the state file. Returns ErrNotInitialized if the project has
// not been created with `maestro init` yet.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.StateFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", s.StateFile(), err)
	}
	return &st, nil
}

// Save writes the state file atomically (write to .tmp then rename).
func (s *Store) Save(st *State) error {
	if err := os.MkdirAll(s.ProjectDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir project: %w", err)
	}
	st.Updated = time.Now()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := s.StateFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}
	if err := os.Rename(tmp, s.StateFile()); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// ErrNotInitialized is returned when a project's state file is missing.
var ErrNotInitialized = errors.New("project not initialized; run `maestro init`")

// FindTask returns the task with the given ID or nil.
func (st *State) FindTask(id string) *Task {
	for _, t := range st.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// RemoveTask drops a task from the slice. Returns true if a task was removed.
// The next-task-number counter is intentionally not rolled back; new tasks
// keep getting fresh IDs even after deletes.
func (st *State) RemoveTask(id string) bool {
	for i, t := range st.Tasks {
		if t.ID == id {
			st.Tasks = append(st.Tasks[:i], st.Tasks[i+1:]...)
			return true
		}
	}
	return false
}

// AllocTaskID hands out the next sequential task ID and bumps the counter on
// the project. The ID format is "t<N>" where N starts at 1.
func (st *State) AllocTaskID() string {
	if st.Project.NextTaskNumber < 1 {
		st.Project.NextTaskNumber = 1
	}
	id := "t" + strconv.Itoa(st.Project.NextTaskNumber)
	st.Project.NextTaskNumber++
	return id
}

// AllocSessionID hands out the next session ID. Format "s<N>".
func (st *State) AllocSessionID() string {
	if st.Project.NextSessionNumber < 1 {
		st.Project.NextSessionNumber = 1
	}
	id := "s" + strconv.Itoa(st.Project.NextSessionNumber)
	st.Project.NextSessionNumber++
	return id
}

// FindSession returns the session with the given ID or nil.
func (st *State) FindSession(id string) *Session {
	for _, s := range st.Sessions {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// TasksInSession returns tasks belonging to the given session, in creation order.
func (st *State) TasksInSession(id string) []*Task {
	var out []*Task
	for _, t := range st.Tasks {
		if t.Session == id {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// AllTags returns a map of tag → number of tasks using it across the project.
func (st *State) AllTags() map[string]int {
	counts := make(map[string]int)
	for _, t := range st.Tasks {
		for _, tag := range t.Tags {
			counts[tag]++
		}
	}
	return counts
}

// SearchQuery is the input to State.SearchTasks. All criteria are AND'd; an
// empty/zero value matches every task. Tags is OR'd within itself (any-of).
type SearchQuery struct {
	Text     string       // case-insensitive substring match against Label/Description/Summary
	Tags     []string     // any-of: matches if the task has at least one of these tags
	Session  string       // exact match on Task.Session
	Statuses []TaskStatus // any-of
	Since    time.Time    // include tasks with UpdatedAt >= Since (zero = no lower bound)
	Until    time.Time    // include tasks with UpdatedAt <= Until (zero = no upper bound)
	Limit    int          // 0 = no limit
}

// SearchTasks returns tasks matching the query, sorted by UpdatedAt descending.
func (st *State) SearchTasks(q SearchQuery) []*Task {
	text := strings.ToLower(strings.TrimSpace(q.Text))
	tagSet := make(map[string]bool, len(q.Tags))
	for _, t := range q.Tags {
		t = strings.TrimSpace(t)
		if t != "" {
			tagSet[t] = true
		}
	}
	statusSet := make(map[TaskStatus]bool, len(q.Statuses))
	for _, s := range q.Statuses {
		statusSet[s] = true
	}

	var out []*Task
	for _, t := range st.Tasks {
		if text != "" {
			hit := strings.Contains(strings.ToLower(t.Label), text) ||
				strings.Contains(strings.ToLower(t.Description), text) ||
				strings.Contains(strings.ToLower(t.Summary), text)
			if !hit {
				continue
			}
		}
		if len(tagSet) > 0 {
			matched := false
			for _, tag := range t.Tags {
				if tagSet[tag] {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if q.Session != "" && t.Session != q.Session {
			continue
		}
		if len(statusSet) > 0 && !statusSet[t.Status] {
			continue
		}
		if !q.Since.IsZero() && t.UpdatedAt.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && t.UpdatedAt.After(q.Until) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// ConflictingTasks returns active tasks (other than excludeID) whose declared
// file lists overlap with the supplied files.
func (st *State) ConflictingTasks(excludeID string, files []string) []*Task {
	if len(files) == 0 {
		return nil
	}
	want := make(map[string]bool, len(files))
	for _, f := range files {
		want[normalizePath(f)] = true
	}
	var out []*Task
	for _, t := range st.Tasks {
		if t.ID == excludeID || !t.Status.IsActive() {
			continue
		}
		for _, f := range t.DeclaredFiles {
			if want[normalizePath(f)] {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// ActiveCount returns the number of tasks currently in flight (any active
// status). Used to enforce the orchestrator's parallelism cap.
func (st *State) ActiveCount() int {
	n := 0
	for _, t := range st.Tasks {
		if t.Status.IsActive() {
			n++
		}
	}
	return n
}

// SortedTasks returns tasks sorted by creation time, most recent last.
func (st *State) SortedTasks() []*Task {
	out := append([]*Task(nil), st.Tasks...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// AddNote appends an audit-trail note (no Type) to a task and bumps UpdatedAt.
// For typed entries (report, exchange, fold, decision), use AddTypedNote.
func (t *Task) AddNote(source, content string) {
	t.AddTypedNote(source, "", content)
}

// AddTypedNote appends a note with an explicit Type field. Type is intended
// for filtering during search and condensation: report (sub-agent's final
// report), exchange (back-and-forth), fold (orchestrator-side refinement),
// decision (constraint or call-out worth keeping past condensation), system.
func (t *Task) AddTypedNote(source, noteType, content string) {
	t.Notes = append(t.Notes, Note{
		At:      time.Now(),
		Source:  source,
		Type:    noteType,
		Content: content,
	})
	t.UpdatedAt = time.Now()
}

// AddTags adds tags to the task, deduplicating.
func (t *Task) AddTags(tags []string) {
	have := make(map[string]bool, len(t.Tags))
	for _, tag := range t.Tags {
		have[tag] = true
	}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || have[tag] {
			continue
		}
		have[tag] = true
		t.Tags = append(t.Tags, tag)
	}
	sort.Strings(t.Tags)
	t.UpdatedAt = time.Now()
}

// RemoveTags drops the supplied tags from the task.
func (t *Task) RemoveTags(tags []string) {
	drop := make(map[string]bool, len(tags))
	for _, tag := range tags {
		drop[strings.TrimSpace(tag)] = true
	}
	kept := t.Tags[:0]
	for _, tag := range t.Tags {
		if !drop[tag] {
			kept = append(kept, tag)
		}
	}
	t.Tags = kept
	t.UpdatedAt = time.Now()
}

// Condense trims the task's verbose fields after its session has been
// summarized into Session.Condensed. ImplementerPrompt is dropped, Notes
// are filtered: Type=decision keeps full content, Type=report keeps the
// first 200 chars (a sentence or two), all other types are dropped. The
// task's metadata (Label, Description, Summary, FinalCommit, Tags,
// DeclaredFiles) is preserved so search and history queries still work.
func (t *Task) Condense() {
	t.ImplementerPrompt = ""
	var kept []Note
	for _, n := range t.Notes {
		switch n.Type {
		case "decision":
			kept = append(kept, n)
		case "report":
			n.Content = truncate(n.Content, 200)
			kept = append(kept, n)
		}
	}
	t.Notes = kept
	t.CondensedAt = time.Now()
	t.UpdatedAt = time.Now()
}

// RenameTagAcrossTasks rewrites every occurrence of `from` to `to` in tag
// lists across all tasks. Useful for canonicalizing drift (auth-flow → auth).
// Returns the number of tasks changed.
func (st *State) RenameTagAcrossTasks(from, to string) int {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return 0
	}
	changed := 0
	for _, t := range st.Tasks {
		hit := false
		for i, tag := range t.Tags {
			if tag == from {
				t.Tags[i] = to
				hit = true
			}
		}
		if !hit {
			continue
		}
		seen := make(map[string]bool, len(t.Tags))
		deduped := t.Tags[:0]
		for _, tag := range t.Tags {
			if !seen[tag] {
				seen[tag] = true
				deduped = append(deduped, tag)
			}
		}
		t.Tags = deduped
		sort.Strings(t.Tags)
		t.UpdatedAt = time.Now()
		changed++
	}
	return changed
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// MergeFiles adds the supplied paths to the declared file set, deduplicating.
func (t *Task) MergeFiles(paths []string) {
	have := make(map[string]bool, len(t.DeclaredFiles))
	for _, f := range t.DeclaredFiles {
		have[normalizePath(f)] = true
	}
	for _, p := range paths {
		n := normalizePath(p)
		if n == "" || have[n] {
			continue
		}
		have[n] = true
		t.DeclaredFiles = append(t.DeclaredFiles, n)
	}
	sort.Strings(t.DeclaredFiles)
	t.UpdatedAt = time.Now()
}

// RemoveFiles drops the supplied paths from the declared file set.
func (t *Task) RemoveFiles(paths []string) {
	drop := make(map[string]bool, len(paths))
	for _, p := range paths {
		drop[normalizePath(p)] = true
	}
	kept := t.DeclaredFiles[:0]
	for _, f := range t.DeclaredFiles {
		if !drop[normalizePath(f)] {
			kept = append(kept, f)
		}
	}
	t.DeclaredFiles = kept
	t.UpdatedAt = time.Now()
}

func normalizePath(p string) string {
	return strings.TrimSpace(filepath.Clean(p))
}

func validateProjectName(name string) error {
	if name == "" {
		return errors.New("empty project name")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("project name %q contains invalid character %q (use alphanumerics, dash, underscore)", name, r)
		}
	}
	return nil
}

// ListProjects scans ~/.maestro/ for initialized projects.
func ListProjects() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".maestro")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), stateFileName)); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ProjectMatch is one row in a `project find --repo` result. Updated is the
// state file's last-modified time, used for sorting most-recent-first so the
// orchestrator can pick the obvious candidate when there are several.
type ProjectMatch struct {
	Name      string    `json:"name"`
	RepoPath  string    `json:"repo_path"`
	Updated   time.Time `json:"updated"`
	SmokeGate string    `json:"smoke_gate,omitempty"`
}

// FindProjectsByRepo returns every project whose RepoPath matches repoPath,
// most-recently-updated first. The path is canonicalized via filepath.Abs +
// EvalSymlinks before comparing so /tmp/foo and /private/tmp/foo match on
// macOS where /tmp is a symlink.
func FindProjectsByRepo(repoPath string) ([]ProjectMatch, error) {
	target, err := canonicalize(repoPath)
	if err != nil {
		return nil, err
	}
	names, err := ListProjects()
	if err != nil {
		return nil, err
	}
	var matches []ProjectMatch
	for _, name := range names {
		s, err := NewStore(name)
		if err != nil {
			continue
		}
		st, err := s.Load()
		if err != nil {
			continue
		}
		canon, err := canonicalize(st.Project.RepoPath)
		if err != nil {
			continue
		}
		if canon == target {
			matches = append(matches, ProjectMatch{
				Name:      st.Project.Name,
				RepoPath:  st.Project.RepoPath,
				Updated:   st.Updated,
				SmokeGate: st.Project.SmokeGate,
			})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Updated.After(matches[j].Updated)
	})
	return matches, nil
}

func canonicalize(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		return eval, nil
	}
	return abs, nil
}

// HasActiveWorktrees reports whether the project's wt/ dir contains any
// task subdirectories. Used as a guardrail for `project rename`, which would
// invalidate git's absolute-path worktree records.
func (s *Store) HasActiveWorktrees() (bool, error) {
	entries, err := os.ReadDir(s.WorktreesDir())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

// Rename moves the project directory and updates the embedded name. Caller
// is responsible for refusing to call this when there are active worktrees;
// see HasActiveWorktrees.
func (s *Store) Rename(newName string) (*Store, error) {
	if err := validateProjectName(newName); err != nil {
		return nil, err
	}
	newStore, err := NewStore(newName)
	if err != nil {
		return nil, err
	}
	if newStore.Exists() {
		return nil, fmt.Errorf("target project %q already exists", newName)
	}
	if err := os.Rename(s.ProjectDir(), newStore.ProjectDir()); err != nil {
		return nil, fmt.Errorf("rename project dir: %w", err)
	}
	st, err := newStore.Load()
	if err != nil {
		return nil, err
	}
	st.Project.Name = newName
	if err := newStore.Save(st); err != nil {
		return nil, err
	}
	return newStore, nil
}
