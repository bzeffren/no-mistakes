//go:build unix

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

// These two regressions need a fake git on PATH that stalls or blocks on a
// chosen command. They follow this package's existing fake-git convention
// (installFakeGit writes a /bin/sh shim, git_unix_test.go), so they are
// unix-only and never compile a shebang shim into a Windows test run where it
// would silently fail to intercept git.

// TestWorktreeInitSubmodulesTimesOutAndPrunesPartialMaterialization proves the
// independent bound (submoduleInitTimeout): when a local Git operation stops
// responding, the call fails with a deadline error instead of hanging forever,
// and the sibling submodule already materialized before the stall is pruned
// again. The fake git blocks far longer than the shortened bound whenever it is
// asked about "sub2", so materialization wedges at sub2's revision lookup -
// after "sub" (which sorts first) is already materialized and recorded. Every
// other command is forwarded to real git so "sub" materializes for real.
func TestWorktreeInitSubmodulesTimesOutAndPrunesPartialMaterialization(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)
	root := filepath.Dir(caller)

	// A second, fully local submodule: its revision lookup would ordinarily
	// succeed, so the only reason it stalls is the fake git's block.
	sub2Bare := filepath.Join(root, "sub2.git")
	if err := InitBare(ctx, sub2Bare); err != nil {
		t.Fatal(err)
	}
	sub2Seed := initTestRepo(t)
	run(t, sub2Seed, "git", "remote", "add", "origin", sub2Bare)
	run(t, sub2Seed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	run(t, sub2Bare, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	runAllow(t, caller, "submodule", "add", sub2Bare, "sub2")
	run(t, caller, "git", "commit", "-q", "-m", "add second submodule")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	// Create the run worktree with the real git, before the fake one is on PATH.
	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// A short bound - generous enough for "sub" to materialize on any machine,
	// far below the fake git's block on "sub2".
	oldTimeout := submoduleInitTimeout
	submoduleInitTimeout = 3 * time.Second
	t.Cleanup(func() { submoduleInitTimeout = oldTimeout })

	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatalf("resolve real git: %v", err)
	}
	t.Setenv("NM_TEST_FAKE_REAL_GIT", realGit)
	installFakeGit(t, `
for arg in "$@"; do
	case "$arg" in
	*sub2*) exec sleep 60 ;;
	esac
done
exec "$NM_TEST_FAKE_REAL_GIT" "$@"
`)

	start := time.Now()
	err = WorktreeInitSubmodules(ctx, caller, wt)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error; the unresponsive local operation was not bounded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error is not a deadline timeout: %v", err)
	}
	// The independent deadline, not a caller cancellation, must be reported: the
	// actionable diagnostic distinguishes a local stall from the caller giving up.
	if !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("timeout diagnostic missing; deadline not distinguished from caller cancel: %v", err)
	}
	if elapsed >= 30*time.Second {
		t.Fatalf("call took %s; the bound did not cut the unresponsive operation short", elapsed)
	}

	// "sub" sorts before "sub2", so it was materialized before sub2 stalled;
	// its worktree registration in the embedded Git directory must be pruned.
	subGitDir := filepath.Join(caller, ".git", "modules", "sub")
	listOut := run(t, subGitDir, "git", "worktree", "list", "--porcelain")
	if strings.Contains(listOut, filepath.Join(wt, "sub")) {
		t.Fatalf("partial materialization was not pruned after the timeout:\n%s", listOut)
	}
}

// TestWorktreeInitSubmodulesCancellationPrunesRegisteredWorktree proves that a
// submodule whose `git worktree add` is killed AFTER it registered the linked
// worktree but before WorktreeInitSubmodules returns is still unregistered by
// cleanup. The fake git forwards the real worktree add (so the registration
// genuinely lands), signals via a marker file, then blocks until the caller
// cancels. Because the submodule is recorded before the add rather than after,
// pruneMaterializedSubmodules removes it; the old order left the registration
// stranded. A caller cancellation must also stay distinct from the independent
// deadline diagnostic.
func TestWorktreeInitSubmodulesCancellationPrunesRegisteredWorktree(t *testing.T) {
	caller, _ := submoduleFixture(t)
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	// Create the run worktree with the real git, before the fake one is on PATH
	// (otherwise this top-level worktree add would itself block).
	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(context.Background(), caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatalf("resolve real git: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "registered")
	t.Setenv("NM_TEST_FAKE_REAL_GIT", realGit)
	t.Setenv("NM_TEST_FAKE_MARKER", marker)
	installFakeGit(t, `
has_worktree=0
has_add=0
for arg in "$@"; do
	case "$arg" in
	worktree) has_worktree=1 ;;
	add) has_add=1 ;;
	esac
done
if [ "$has_worktree" = 1 ] && [ "$has_add" = 1 ]; then
	"$NM_TEST_FAKE_REAL_GIT" "$@"
	printf 'ready\n' > "$NM_TEST_FAKE_MARKER"
	while :; do sleep 1; done
fi
exec "$NM_TEST_FAKE_REAL_GIT" "$@"
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var initErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		initErr = WorktreeInitSubmodules(ctx, caller, wt)
	}()

	waitForFile(t, marker, 30*time.Second) // the worktree registration has landed
	cancel()
	<-done

	if initErr == nil {
		t.Fatal("expected a cancellation error while the worktree add was blocked")
	}
	if !errors.Is(initErr, context.Canceled) {
		t.Fatalf("error is not a caller cancellation: %v", initErr)
	}
	if strings.Contains(initErr.Error(), "did not finish within") {
		t.Fatalf("caller cancellation was misreported as the independent deadline: %v", initErr)
	}

	// The submodule whose worktree add was killed after it registered must have
	// been unregistered again; the run worktree's partial content must be gone.
	subGitDir := filepath.Join(caller, ".git", "modules", "sub")
	listOut := run(t, subGitDir, "git", "worktree", "list", "--porcelain")
	if strings.Contains(listOut, filepath.Join(wt, "sub")) {
		t.Fatalf("registered submodule worktree was not pruned after cancellation:\n%s", listOut)
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "README.md")); err == nil {
		t.Fatal("the partial run worktree submodule content was not removed after cancellation")
	}
}
