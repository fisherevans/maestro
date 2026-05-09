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
	Project Project   `json:"project"`
	Tasks   []*Task   `json:"tasks"`
	Updated time.Time `json:"updated"`
}

// Project holds the immutable-ish config for a maestro project: which repo
// it tracks and what branch new tasks default to.
type Project struct {
	Name           string `json:"name"`
	RepoPath       string `json:"repo_path"`
	DefaultBase    string `json:"default_base"`
	NextTaskNumber int    `json:"next_task_number"`
}

// Task is one unit of work assigned to a sub-agent.
type Task struct {
	ID            string     `json:"id"`
	Description   string     `json:"description"`
	Status        TaskStatus `json:"status"`
	Branch        string     `json:"branch"`
	BaseBranch    string     `json:"base_branch"`
	BaseCommit    string     `json:"base_commit"`
	WorktreePath  string     `json:"worktree_path"`
	DeclaredFiles []string   `json:"declared_files"`
	AgentID       string     `json:"agent_id"`
	Summary       string     `json:"summary"`
	FinalCommit   string     `json:"final_commit"`
	Notes         []Note     `json:"notes"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Note is a timestamped log entry on a task. Used as a simple audit trail so
// the orchestrator can recover context after a long session.
type Note struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
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

// AddNote appends an audit-trail note to a task and bumps UpdatedAt.
func (t *Task) AddNote(source, content string) {
	t.Notes = append(t.Notes, Note{
		At:      time.Now(),
		Source:  source,
		Content: content,
	})
	t.UpdatedAt = time.Now()
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
