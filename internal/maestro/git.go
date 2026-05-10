package maestro

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git is a thin wrapper around shelling out to `git` with a fixed working
// directory. We don't pull in a Go git library because we only need a handful
// of plumbing commands and the failure modes are easier to reason about when
// you can copy-paste them into a terminal.
type Git struct {
	RepoPath string
}

// Run executes `git <args...>` in g.RepoPath and returns trimmed stdout.
// Stderr is folded into the error on failure so callers see what git complained about.
func (g *Git) Run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.RepoPath
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return trimmed, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, trimmed)
	}
	return trimmed, nil
}

// IsRepo reports whether g.RepoPath is inside a git working tree.
func (g *Git) IsRepo() bool {
	_, err := g.Run("rev-parse", "--is-inside-work-tree")
	return err == nil
}

// Toplevel returns the absolute path of the repo root.
func (g *Git) Toplevel() (string, error) {
	return g.Run("rev-parse", "--show-toplevel")
}

// ResolveSHA returns the full commit SHA for a ref.
func (g *Git) ResolveSHA(ref string) (string, error) {
	return g.Run("rev-parse", ref)
}

// CurrentBranch returns the short name of the currently checked-out branch.
// Returns "HEAD" if detached.
func (g *Git) CurrentBranch() (string, error) {
	return g.Run("rev-parse", "--abbrev-ref", "HEAD")
}

// BranchExists reports whether a local branch with the given name exists.
func (g *Git) BranchExists(name string) bool {
	_, err := g.Run("rev-parse", "--verify", "refs/heads/"+name)
	return err == nil
}

// CreateWorktree creates a new worktree at path on a new branch forked from
// base. Equivalent to `git worktree add -b <branch> <path> <base>`. Creates
// any missing parent directories.
func (g *Git) CreateWorktree(path, branch, base string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if _, err := g.Run("worktree", "add", "-b", branch, path, base); err != nil {
		return err
	}
	return nil
}

// AttachWorktree creates a worktree at path that checks out an existing
// branch. Used by `worktree restore` to recover a cleaned-up workspace
// without inventing a new branch.
func (g *Git) AttachWorktree(path, branch string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if _, err := g.Run("worktree", "add", path, branch); err != nil {
		return err
	}
	return nil
}

// RemoveWorktree removes the worktree directory and prunes the registration.
// Pass force=true to remove a worktree with uncommitted changes.
func (g *Git) RemoveWorktree(path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := g.Run(args...); err != nil {
		// `git worktree remove` complains if the worktree directory is gone
		// already; in that case, prune and move on.
		if _, prr := g.Run("worktree", "prune"); prr == nil {
			if _, ferr := os.Stat(path); os.IsNotExist(ferr) {
				return nil
			}
		}
		return err
	}
	return nil
}

// CommitsAhead returns how many commits `branch` is ahead of `base`. Returns
// (0, nil) if branch == base or if either ref is missing.
func (g *Git) CommitsAhead(base, branch string) (int, error) {
	out, err := g.Run("rev-list", "--count", base+".."+branch)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", out, err)
	}
	return n, nil
}

// CommitsBehind returns how many commits `branch` is behind `base`.
func (g *Git) CommitsBehind(base, branch string) (int, error) {
	return g.CommitsAhead(branch, base)
}
