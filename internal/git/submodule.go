package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// WorktreeInitSubmodules materializes wt's committed submodules, recursively,
// using only Git objects already present on the machine: each submodule's own
// embedded Git directory under sourceDir's shared ".../modules/<name>" tree.
//
// sourceDir must be the repository's registered working copy
// (repo.WorkingPath), never the bare gate a pushed branch lands in: a plain
// `git push` never transfers a submodule's own object store, so the gate -
// populated purely from received pushes - never has one. The operator's own
// normal working copy does, exactly like it has the top-level repository's
// own object store, because the operator runs `git submodule update --init`
// there as part of their normal workflow.
//
// It makes no DNS lookup and opens no network connection - a submodule that
// was never initialized, or whose committed revision was never fetched, in
// sourceDir fails immediately with an actionable diagnostic instead of
// fetching one. A repository with no .gitmodules is a successful no-op.
//
// This reads sourceDir only (rev-parse and its shared modules/ tree); it
// never runs a submodule command there, so a Treehouse-style submodule
// symlink checkout at sourceDir is untouched. On any failure, every
// submodule already materialized during this call is unregistered again
// before returning, so a partial recursive failure leaves no stale worktree
// metadata in sourceDir's own submodule stores.
func WorktreeInitSubmodules(ctx context.Context, sourceDir, wt string) error {
	gitDir, err := Run(ctx, sourceDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("resolve git directory for %s: %w", sourceDir, err)
	}
	var created []materializedSubmodule
	if err := initSubmodulesLevel(ctx, gitDir, wt, &created); err != nil {
		pruneMaterializedSubmodules(ctx, created)
		return err
	}
	return nil
}

type materializedSubmodule struct {
	gitDir string
	path   string
}

// initSubmodulesLevel materializes the submodules committed directly in wt
// (already checked out at some revision), using parentGitDir - the Git
// directory that owns wt - to locate each submodule's embedded Git
// directory. It then recurses into each materialized submodule using its own
// embedded Git directory as the next level's parentGitDir. Every submodule it
// successfully materializes is appended to *created, in creation order, so a
// later failure can unregister them in reverse.
func initSubmodulesLevel(ctx context.Context, parentGitDir, wt string, created *[]materializedSubmodule) error {
	// .gitmodules is read as the committed tree entry, never as a worktree
	// filesystem path: a pushed branch's .gitmodules could otherwise be a
	// symlink, or use Git config's [include]/[includeIf], to redirect this
	// read somewhere outside the intended file. This mirrors this repo's own
	// trusted-tree-entry convention for other security-sensitive paths
	// (pr.template).
	if _, err := Run(ctx, wt, "rev-parse", "--verify", "--quiet", "HEAD:.gitmodules"); err != nil {
		return nil
	}

	paths, err := submodulePathsByName(ctx, wt)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf(".gitmodules is committed but declares no valid submodule path entries")
	}

	// Deterministic order: a fixed, name-sorted processing order makes a
	// partial-failure prune reproducible instead of depending on Go's
	// randomized map iteration.
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		path := paths[name]
		if err := validateSubmoduleNameAndPath(name, path); err != nil {
			return err
		}

		rev, err := gitlinkRevision(ctx, wt, path)
		if err != nil {
			return err
		}
		if rev == "" {
			// .gitmodules names it, but it is not a gitlink in the checked-out
			// tree (e.g. removed on this revision without dropping the
			// section yet). Nothing to materialize.
			continue
		}

		modulesRoot := filepath.Join(parentGitDir, "modules")
		subGitDir := filepath.Join(modulesRoot, name)
		if !worktrees.Contains(modulesRoot, subGitDir) {
			return fmt.Errorf("submodule %q resolves outside the local modules directory", name)
		}
		if info, statErr := os.Stat(subGitDir); statErr != nil || !info.IsDir() {
			return fmt.Errorf("submodule %q is not available locally (no repository at %s): initialize or fetch it in the repository's normal trusted working copy first", name, subGitDir)
		}

		subPath := filepath.Join(wt, path)
		if !worktrees.Contains(wt, subPath) {
			return fmt.Errorf("submodule %q path resolves outside the run worktree", name)
		}

		if err := worktreeAddFromGitDir(ctx, subGitDir, subPath, rev); err != nil {
			return fmt.Errorf("submodule %q revision %s is not available locally: initialize or fetch it in the repository's normal trusted working copy first: %w", name, rev, err)
		}
		*created = append(*created, materializedSubmodule{gitDir: subGitDir, path: subPath})

		if err := initSubmodulesLevel(ctx, subGitDir, subPath, created); err != nil {
			return err
		}
	}
	return nil
}

// pruneMaterializedSubmodules unregisters every submodule worktree created
// during a failed WorktreeInitSubmodules call, in reverse creation order, so
// a parent's own directory removal is never the only cleanup a caller relies
// on: the submodule's embedded Git directory otherwise keeps a stale
// worktree registration (and its retained objects) after the run worktree
// that pointed at it is gone. Best-effort: a removal failure is not
// escalated, since the caller is already returning the original error.
func pruneMaterializedSubmodules(ctx context.Context, created []materializedSubmodule) {
	for i := len(created) - 1; i >= 0; i-- {
		m := created[i]
		_, _ = Run(ctx, m.gitDir, "--git-dir="+m.gitDir, "worktree", "remove", "--force", m.path)
	}
}

// validateSubmoduleNameAndPath rejects a .gitmodules name or path that could
// escape its intended directory (the CVE-2018-11235 class of bug): empty,
// absolute, or containing a "." or ".." path component. Git's own submodule
// tooling applies an equivalent check to name; this design also joins path
// into a filesystem path, so it gets the same treatment.
func validateSubmoduleNameAndPath(name, path string) error {
	if err := rejectUnsafeRelativePath(name); err != nil {
		return fmt.Errorf("submodule name %q is not allowed: %w", name, err)
	}
	if err := rejectUnsafeRelativePath(path); err != nil {
		return fmt.Errorf("submodule path %q is not allowed: %w", path, err)
	}
	return nil
}

func rejectUnsafeRelativePath(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if filepath.IsAbs(s) || strings.HasPrefix(filepath.ToSlash(s), "/") {
		return fmt.Errorf("must be a relative path")
	}
	for _, part := range strings.Split(filepath.ToSlash(s), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("must not contain an empty, %q, or %q path component", ".", "..")
		}
	}
	return nil
}

// submodulePathsByName reads name -> path for every submodule section in
// HEAD's committed .gitmodules blob, via `git config --blob`, which never
// touches the worktree filesystem. It intentionally never reads the URL:
// this design never fetches, so a submodule's remote URL is irrelevant to
// materializing it.
func submodulePathsByName(ctx context.Context, wt string) (map[string]string, error) {
	out, err := Run(ctx, wt, "config", "--blob", "HEAD:.gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		if isGitConfigNoMatch(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read submodule paths from .gitmodules: %w", err)
	}

	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		key, path, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		if name == "" || path == "" {
			continue
		}
		result[name] = path
	}
	return result, nil
}

func isGitConfigNoMatch(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// gitlinkRevision returns the exact commit SHA committed at path in wt's
// checked-out HEAD, or "" if path is not a gitlink there.
func gitlinkRevision(ctx context.Context, wt, path string) (string, error) {
	out, err := Run(ctx, wt, "ls-tree", "HEAD", "--", path)
	if err != nil {
		return "", fmt.Errorf("read committed submodule revision for %s: %w", path, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	fields := strings.Fields(out)
	if len(fields) < 4 || fields[1] != "commit" {
		return "", fmt.Errorf("path %s is committed but is not a submodule gitlink", path)
	}
	return fields[2], nil
}

// worktreeAddFromGitDir materializes sha into wtPath as a linked worktree of
// the repository at gitDir, using only gitDir's own local object store.
//
// Like WorktreeAdd, this performs no fetch or clone for a full local object
// store: git worktree add resolves sha against already-present local refs
// and objects only. GIT_NO_LAZY_FETCH additionally fails closed rather than
// lazily fetching a missing object if gitDir happens to be a partial
// (promisor) clone. core.hooksPath is pointed at an empty directory so a
// hook committed in - or already present in - the embedded repository cannot
// run as a side effect of this checkout, and the well-known Git LFS filter
// names are neutralized to a no-op pass-through so a pushed .gitattributes
// cannot select a locally configured LFS filter to fetch objects over the
// network this design otherwise never touches.
func worktreeAddFromGitDir(ctx context.Context, gitDir, wtPath, sha string) error {
	noHooksDir, err := os.MkdirTemp("", "no-mistakes-no-hooks-")
	if err != nil {
		return fmt.Errorf("prepare hook isolation: %w", err)
	}
	defer os.RemoveAll(noHooksDir)

	_, err = RunWithEnv(ctx, gitDir, []string{"GIT_NO_LAZY_FETCH=1"},
		"--git-dir="+gitDir,
		"-c", "core.hooksPath="+noHooksDir,
		"-c", "filter.lfs.clean=cat",
		"-c", "filter.lfs.smudge=cat",
		"-c", "filter.lfs.process=",
		"worktree", "add", "--detach", "--", wtPath, sha,
	)
	return err
}
