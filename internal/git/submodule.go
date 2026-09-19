package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeInitSubmodules materializes wt's committed submodules, recursively,
// using only Git objects already present on the machine: each submodule's own
// embedded Git directory under gateDir's ".../modules/<name>" tree, shared by
// every worktree of gateDir. It makes no DNS lookup and opens no network
// connection - a submodule that was never initialized, or whose committed
// revision was never fetched, in the repository's normal trusted working copy
// fails immediately with an actionable diagnostic instead of fetching one. A
// repository with no .gitmodules is a successful no-op.
//
// This never runs a submodule command in gateDir itself; it only reads
// gateDir's already-present object stores and writes into wt.
func WorktreeInitSubmodules(ctx context.Context, gateDir, wt string) error {
	gitDir, err := Run(ctx, gateDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("resolve git directory for %s: %w", gateDir, err)
	}
	return initSubmodulesLevel(ctx, gitDir, wt)
}

// initSubmodulesLevel materializes the submodules committed directly in wt
// (already checked out at some revision), using parentGitDir - the Git
// directory that owns wt - to locate each submodule's embedded Git
// directory. It then recurses into each materialized submodule using its own
// embedded Git directory as the next level's parentGitDir.
func initSubmodulesLevel(ctx context.Context, parentGitDir, wt string) error {
	if _, err := os.Stat(filepath.Join(wt, ".gitmodules")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat .gitmodules in %s: %w", wt, err)
	}

	paths, err := submodulePathsByName(ctx, wt)
	if err != nil {
		return err
	}

	for name, path := range paths {
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

		subGitDir := filepath.Join(parentGitDir, "modules", name)
		if info, statErr := os.Stat(subGitDir); statErr != nil || !info.IsDir() {
			return fmt.Errorf("submodule %q is not available locally (no repository at %s): initialize or fetch it in the repository's normal trusted working copy first", name, subGitDir)
		}

		subPath := filepath.Join(wt, path)
		if err := worktreeAddFromGitDir(ctx, subGitDir, subPath, rev); err != nil {
			return fmt.Errorf("submodule %q revision %s is not available locally: initialize or fetch it in the repository's normal trusted working copy first: %w", name, rev, err)
		}

		if err := initSubmodulesLevel(ctx, subGitDir, subPath); err != nil {
			return err
		}
	}
	return nil
}

// submodulePathsByName reads name -> path for every submodule section in the
// checked-out .gitmodules, purely locally. It intentionally never reads the
// URL: this design never fetches, so a submodule's remote URL is irrelevant
// to materializing it.
func submodulePathsByName(ctx context.Context, wt string) (map[string]string, error) {
	out, err := Run(ctx, wt, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// No matching keys - .gitmodules has no valid submodule sections.
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
// the repository at gitDir, using only gitDir's own local object store. Like
// WorktreeAdd, it performs no fetch or clone: git worktree add resolves sha
// against already-present local refs and objects only, so a missing gitDir
// or a missing sha both fail immediately with no network access attempted.
func worktreeAddFromGitDir(ctx context.Context, gitDir, wtPath, sha string) error {
	_, err := Run(ctx, gitDir, "--git-dir="+gitDir, "worktree", "add", "--detach", wtPath, sha)
	return err
}
