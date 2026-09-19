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
	"time"

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
		pruneMaterializedSubmodules(created)
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
	gitlinks, err := gitlinkPathsInTree(ctx, wt)
	if err != nil {
		return err
	}
	if len(gitlinks) == 0 {
		// No committed gitlink at all: a genuinely submodule-free tree,
		// regardless of whether a stray .gitmodules happens to exist.
		return nil
	}

	// .gitmodules is read as the committed tree entry, never as a worktree
	// filesystem path: a pushed branch's .gitmodules could otherwise be a
	// symlink, or use Git config's [include]/[includeIf], to redirect this
	// read somewhere outside the intended file. This mirrors this repo's own
	// trusted-tree-entry convention for other security-sensitive paths
	// (pr.template).
	if _, err := Run(ctx, wt, "rev-parse", "--verify", "--quiet", "HEAD:.gitmodules"); err != nil {
		return fmt.Errorf("commit has %d submodule gitlink(s) but no .gitmodules file at all: initialize or fetch them in the repository's normal trusted working copy first", len(gitlinks))
	}

	paths, err := submodulePathsByName(ctx, wt)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf(".gitmodules is committed but declares no valid submodule path entries")
	}
	if err := requireEveryGitlinkIsDeclared(gitlinks, paths); err != nil {
		return err
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
// that pointed at it is gone.
//
// It uses its own short-lived, independent context rather than the caller's:
// the caller's context may itself be why initSubmodulesLevel failed (for
// example daemon shutdown), and reusing an already-canceled context here
// would make cleanup fail closed exactly when it is most needed.
// Best-effort: a removal failure is not escalated, since the caller is
// already returning the original error.
func pruneMaterializedSubmodules(created []materializedSubmodule) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := len(created) - 1; i >= 0; i-- {
		m := created[i]
		_, _ = Run(cleanupCtx, m.gitDir, "--git-dir="+m.gitDir, "worktree", "remove", "--force", m.path)
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

// gitlinkPathsInTree returns every mode-160000 (gitlink) path committed in
// wt's checked-out HEAD, read via `git ls-tree -r HEAD` with no pathspec
// argument, so no path in the result is subject to Git's pathspec magic
// interpretation (unlike a pathspec passed as a command argument).
func gitlinkPathsInTree(ctx context.Context, wt string) (map[string]bool, error) {
	out, err := Run(ctx, wt, "ls-tree", "-r", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("list committed gitlinks: %w", err)
	}
	result := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		meta, path, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 2 || fields[0] != "160000" || fields[1] != "commit" {
			continue
		}
		result[path] = true
	}
	return result, nil
}

// requireEveryGitlinkIsDeclared cross-checks gitlinks, the committed tree's
// own gitlink paths, against declaredPaths, .gitmodules's valid path
// entries. A .gitmodules with a mix of well-formed and malformed sections
// (for example a section with a url but no path key) would otherwise let a
// real, committed gitlink simply never appear in declaredPaths - silently
// skipped rather than materialized, letting the pipeline run against an
// incomplete checkout. Any gitlink not covered by a declared path is a hard
// error.
func requireEveryGitlinkIsDeclared(gitlinks map[string]bool, declaredPaths map[string]string) error {
	declared := make(map[string]bool, len(declaredPaths))
	for _, p := range declaredPaths {
		declared[p] = true
	}
	for path := range gitlinks {
		if !declared[path] {
			return fmt.Errorf(".gitmodules is malformed or incomplete: commit has a submodule gitlink at %q with no valid .gitmodules path entry naming it", path)
		}
	}
	return nil
}

// gitlinkRevision returns the exact commit SHA committed at path in wt's
// checked-out HEAD, or "" if path is not a gitlink there.
//
// path is passed with a ":(literal)" pathspec prefix so a .gitmodules value
// that happens to look like Git pathspec magic (for example a leading ":")
// is matched as the literal path it names, not reinterpreted - otherwise a
// declared path that legitimately passed the tree cross-check could still
// resolve to the wrong entry, or none, here.
func gitlinkRevision(ctx context.Context, wt, path string) (string, error) {
	out, err := Run(ctx, wt, "ls-tree", "HEAD", "--", ":(literal)"+path)
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
// (promisor) clone.
//
// core.hooksPath is pointed at an empty directory so a hook already present
// in the embedded repository cannot run as a side effect of this checkout.
// GIT_CONFIG_NOSYSTEM plus a throwaway, guaranteed-empty GIT_CONFIG_GLOBAL
// exclude every system- or global-configured setting from this one
// invocation - not just a well-known filter name such as Git LFS's - so a
// pushed .gitattributes cannot select an operator-configured filter to run
// an arbitrary command or fetch over the network this design otherwise
// never touches. GIT_CONFIG_COUNT=0 closes the same door for Git's separate
// GIT_CONFIG_KEY_n/VALUE_n indexed-config environment mechanism. A filter or
// hook defined only in the embedded repository's own local config
// (gitDir/config) is unaffected: that is a value the operator set on that
// specific submodule, not something a pushed branch can reach.
func worktreeAddFromGitDir(ctx context.Context, gitDir, wtPath, sha string) error {
	isolationDir, err := os.MkdirTemp("", "no-mistakes-submodule-isolation-")
	if err != nil {
		return fmt.Errorf("prepare hook and config isolation: %w", err)
	}
	defer os.RemoveAll(isolationDir)

	noHooksDir := filepath.Join(isolationDir, "hooks")
	if err := os.Mkdir(noHooksDir, 0o700); err != nil {
		return fmt.Errorf("prepare hook isolation: %w", err)
	}
	emptyGlobalConfig := filepath.Join(isolationDir, "empty-gitconfig")
	if err := os.WriteFile(emptyGlobalConfig, nil, 0o600); err != nil {
		return fmt.Errorf("prepare hermetic config isolation: %w", err)
	}

	_, err = RunWithEnv(ctx, gitDir, []string{
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + emptyGlobalConfig,
		"GIT_CONFIG_COUNT=0",
	},
		"--git-dir="+gitDir,
		"-c", "core.hooksPath="+noHooksDir,
		"worktree", "add", "--detach", "--", wtPath, sha,
	)
	return err
}
