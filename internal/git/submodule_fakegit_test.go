package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// The submodule provisioning timeout and cancellation regressions need a fake
// git on PATH that stalls or blocks one chosen command. It is a tiny compiled
// Go helper (internal/git/fakegit) built once per test process and linked onto
// PATH as git, following internal/pipeline/fakecli - so the same behavior is
// exercised on Windows and Unix rather than through a /bin/sh-only shim.

var (
	fakeGitHelperPath string
	fakeGitBuildErr   error
)

// buildFakeGitHelper compiles internal/git/fakegit once and records the binary
// path in fakeGitHelperPath. It is invoked from TestMain and its cleanup runs
// after the suite. A build failure is recorded, not fatal, so it only fails the
// tests that actually need the helper.
func buildFakeGitHelper() (func(), error) {
	root, err := findGitModuleRoot()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "nm-git-fakegit-*")
	if err != nil {
		return nil, err
	}
	name := "fakegit"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./internal/git/fakegit")
	cmd.Dir = root
	cmd.Env = fakeGitBuildEnv()
	if b, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("go build fakegit: %w: %s", err, b)
	}
	fakeGitHelperPath = out
	return func() {
		fakeGitHelperPath = ""
		_ = os.RemoveAll(dir)
	}, nil
}

// fakeGitBuildEnv strips -race from GOFLAGS (it requires cgo, which the next
// line disables) and forces CGO_ENABLED=0 so the helper builds fast, matching
// internal/pipeline/fakecli's non-race build.
func fakeGitBuildEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "GOFLAGS") {
			fields := strings.Fields(val)
			kept := fields[:0]
			for _, f := range fields {
				if f == "-race" || f == "--race" {
					continue
				}
				kept = append(kept, f)
			}
			if len(kept) == 0 {
				continue
			}
			out = append(out, key+"="+strings.Join(kept, " "))
			continue
		}
		out = append(out, entry)
	}
	return append(out, "CGO_ENABLED=0")
}

func findGitModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from %s", dir)
		}
		dir = parent
	}
}

// installCompiledFakeGit links the compiled fake git into a fresh PATH entry as
// git (git.exe on Windows) and configures its behavior through the environment.
// Real git is resolved by absolute path so the fake forwards to it without
// re-entering itself via PATH.
func installCompiledFakeGit(t *testing.T, env map[string]string) {
	t.Helper()
	if fakeGitHelperPath == "" {
		if fakeGitBuildErr != nil {
			t.Fatalf("fake git helper not built: %v", fakeGitBuildErr)
		}
		t.Fatal("fake git helper not built")
	}
	real, err := testgit.RealGit()
	if err != nil {
		t.Fatalf("resolve real git: %v", err)
	}
	binDir := t.TempDir()
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	dst := filepath.Join(binDir, name)
	if runtime.GOOS == "darwin" {
		// One executable path keeps macOS code-signature validation happy.
		if err := os.Symlink(fakeGitHelperPath, dst); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Link(fakeGitHelperPath, dst); err != nil {
		data, readErr := os.ReadFile(fakeGitHelperPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("NM_FAKE_GIT_REAL", real)
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// worktreeListRegistersPath reports whether porcelainOut, the output of
// `git worktree list --porcelain`, registers a linked worktree at wantPath.
// Each record begins with a "worktree <path>" line; the recorded path is
// compared to wantPath through the repository's canonical path convention
// (worktrees.Canonical), after filepath.FromSlash normalizes the record's
// separators. git prints the worktree path with forward slashes on Windows,
// so a raw strings.Contains over the porcelain text reports a genuine Windows
// registration as missing - git's slashes never match filepath.Join's
// backslashes. An exact canonical comparison is separator- and symlink-correct
// on every platform (git records the symlink-resolved real path, which on
// macOS differs from t.TempDir()'s /var spelling).
func worktreeListRegistersPath(porcelainOut, wantPath string) bool {
	want := worktrees.Canonical(wantPath)
	for _, line := range strings.Split(porcelainOut, "\n") {
		record, path, ok := strings.Cut(strings.TrimRight(line, "\r"), " ")
		if !ok || record != "worktree" {
			continue
		}
		if worktrees.Canonical(filepath.FromSlash(path)) == want {
			return true
		}
	}
	return false
}

// TestWorktreeInitSubmodulesTimesOutAndPrunesPartialMaterialization proves the
// independent bound (submoduleInitTimeout): when a local Git operation stops
// responding, the call fails with a deadline error instead of hanging forever,
// and the sibling submodule already materialized before the stall is pruned
// again. The compiled fake git blocks far longer than the shortened bound
// whenever it is asked about "sub2", so materialization wedges at sub2's
// revision lookup - after "sub" (which sorts first) is already materialized and
// recorded. Every other command is forwarded to real git so "sub" materializes.
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

	installCompiledFakeGit(t, map[string]string{
		"NM_FAKE_GIT_MODE":  "stall",
		"NM_FAKE_GIT_MATCH": "sub2",
	})

	start := time.Now()
	err := WorktreeInitSubmodules(ctx, caller, wt)
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
	if worktreeListRegistersPath(listOut, filepath.Join(wt, "sub")) {
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

	marker := filepath.Join(t.TempDir(), "registered")
	installCompiledFakeGit(t, map[string]string{
		"NM_FAKE_GIT_MODE":  "register-block",
		"NM_FAKE_GIT_READY": marker,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var initErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		initErr = WorktreeInitSubmodules(ctx, caller, wt)
	}()

	waitForFile(t, marker, 30*time.Second) // the worktree registration has landed

	// The marker is now written only after the forwarded `git worktree add`
	// succeeded, so the linked-worktree registration must be present before we
	// cancel. Assert it here: otherwise a failed registration would leave the
	// post-cancel absence check passing without exercising cleanup at all.
	subGitDir := filepath.Join(caller, ".git", "modules", "sub")
	if listOut := run(t, subGitDir, "git", "worktree", "list", "--porcelain"); !worktreeListRegistersPath(listOut, filepath.Join(wt, "sub")) {
		t.Fatalf("submodule worktree was not registered before cancellation:\n%s", listOut)
	}

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
	listOut := run(t, subGitDir, "git", "worktree", "list", "--porcelain")
	if worktreeListRegistersPath(listOut, filepath.Join(wt, "sub")) {
		t.Fatalf("registered submodule worktree was not pruned after cancellation:\n%s", listOut)
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "README.md")); err == nil {
		t.Fatal("the partial run worktree submodule content was not removed after cancellation")
	}
}
