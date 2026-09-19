package git

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// runAllow runs git with protocol.file.allow=always, for the local file://
// submodule fixtures these tests build; production traffic never uses this.
func runAllow(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "protocol.file.allow=always"}, args...)
	return run(t, dir, "git", full...)
}

// submoduleFixture builds a bare submodule repository (subBare) with one
// commit, a bare parent repository (parentBare) that commits it as a
// submodule at path "sub", and a "caller" clone of parentBare with the
// submodule genuinely fetched - representing the repository's normal
// trusted working copy, whose shared Git directory a run worktree is
// created from. It returns the caller clone's path and the sub commit SHA.
func submoduleFixture(t *testing.T) (caller, subSHA string) {
	t.Helper()
	root := t.TempDir()

	subBare := filepath.Join(root, "sub.git")
	if err := InitBare(context.Background(), subBare); err != nil {
		t.Fatal(err)
	}
	subSeed := initTestRepo(t)
	run(t, subSeed, "git", "remote", "add", "origin", subBare)
	run(t, subSeed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	run(t, subBare, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	subSHA = run(t, subSeed, "git", "rev-parse", "HEAD")

	parentBare := filepath.Join(root, "parent.git")
	if err := InitBare(context.Background(), parentBare); err != nil {
		t.Fatal(err)
	}
	parentSeed := initTestRepo(t)
	runAllow(t, parentSeed, "submodule", "add", "-q", subBare, "sub")
	run(t, parentSeed, "git", "commit", "-q", "-m", "add submodule")
	run(t, parentSeed, "git", "remote", "add", "origin", parentBare)
	run(t, parentSeed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	run(t, parentBare, "git", "symbolic-ref", "HEAD", "refs/heads/main")

	caller = filepath.Join(root, "caller")
	runAllow(t, root, "clone", "-q", "--recurse-submodules", parentBare, caller)
	return caller, subSHA
}

func TestWorktreeInitSubmodulesMaterializesCommittedGitlinkRevision(t *testing.T) {
	ctx := context.Background()
	caller, subSHA := submoduleFixture(t)
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "README.md")); err == nil {
		t.Fatal("submodule should be empty right after plain worktree add")
	}

	if err := WorktreeInitSubmodules(ctx, caller, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules: %v", err)
	}

	got := run(t, filepath.Join(wt, "sub"), "git", "rev-parse", "HEAD")
	if got != subSHA {
		t.Fatalf("materialized submodule at %s, want %s", got, subSHA)
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "README.md")); err != nil {
		t.Fatalf("submodule content not materialized: %v", err)
	}
}

func TestWorktreeInitSubmodulesNestedSubmodules(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)

	// Add a nested submodule inside sub, push it, and advance the caller's
	// own already-fetched sub checkout so its embedded Git directory has the
	// nested submodule's object store too - the trusted working copy having
	// done its own recursive sync.
	root := filepath.Dir(caller)
	grandchildBare := filepath.Join(root, "grandchild.git")
	if err := InitBare(ctx, grandchildBare); err != nil {
		t.Fatal(err)
	}
	gcSeed := initTestRepo(t)
	run(t, gcSeed, "git", "remote", "add", "origin", grandchildBare)
	run(t, gcSeed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	run(t, grandchildBare, "git", "symbolic-ref", "HEAD", "refs/heads/main")

	subPath := filepath.Join(caller, "sub")
	runAllow(t, subPath, "submodule", "add", "-q", grandchildBare, "nested")
	run(t, subPath, "git", "commit", "-q", "-m", "add nested submodule")
	run(t, subPath, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	subSHA := run(t, subPath, "git", "rev-parse", "HEAD")
	runAllow(t, subPath, "submodule", "update", "-q", "--init")

	run(t, caller, "git", "add", "sub")
	run(t, caller, "git", "commit", "-q", "-m", "advance sub to nested-carrying commit")
	run(t, caller, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	if err := WorktreeInitSubmodules(ctx, caller, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules: %v", err)
	}

	if got := run(t, filepath.Join(wt, "sub"), "git", "rev-parse", "HEAD"); got != subSHA {
		t.Fatalf("sub materialized at %s, want %s", got, subSHA)
	}
	nested := filepath.Join(wt, "sub", "nested")
	if _, err := os.Stat(filepath.Join(nested, "README.md")); err != nil {
		t.Fatalf("nested submodule content not materialized: %v", err)
	}
}

func TestWorktreeInitSubmodulesNoGitmodulesIsNoOp(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, src, "git", "remote", "add", "origin", bare)
	run(t, src, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	sha := run(t, src, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, bare, wt, sha); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, bare, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules on a repository with no .gitmodules should be a no-op, got: %v", err)
	}
}

func TestWorktreeInitSubmodulesMissingLocalRepositoryFailsClosed(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	// Simulate a submodule that was never initialized in the trusted working
	// copy at all: remove its embedded Git directory before materializing.
	if err := os.RemoveAll(filepath.Join(caller, ".git", "modules", "sub")); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	err := WorktreeInitSubmodules(ctx, caller, wt)
	if err == nil {
		t.Fatal("expected a clear failure for a submodule never initialized locally")
	}
	if _, statErr := os.Stat(filepath.Join(wt, "sub", "README.md")); statErr == nil {
		t.Fatal("submodule content must not appear when materialization fails closed")
	}
}

func TestWorktreeInitSubmodulesMissingCommitFailsClosed(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)

	// Advance the true submodule remote with a commit the caller's own
	// already-fetched embedded Git directory has never seen or fetched.
	root := filepath.Dir(caller)
	subBare := filepath.Join(root, "sub.git")
	subSeed2 := filepath.Join(root, "sub-seed-2")
	runAllow(t, root, "clone", "-q", subBare, subSeed2)
	run(t, subSeed2, "git", "config", "user.email", "test@test.com")
	run(t, subSeed2, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(subSeed2, "extra.txt"), "extra\n")
	run(t, subSeed2, "git", "add", ".")
	run(t, subSeed2, "git", "commit", "-q", "-m", "never fetched into the caller's local store")
	run(t, subSeed2, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	newSubSHA := run(t, subSeed2, "git", "rev-parse", "HEAD")

	// A gitlink is just a raw SHA reference: set the parent's committed
	// gitlink to newSubSHA via plumbing, without ever fetching that
	// revision into the caller's own embedded Git directory for sub.
	run(t, caller, "git", "update-index", "--cacheinfo", "160000,"+newSubSHA+",sub")
	run(t, caller, "git", "commit", "-q", "-m", "repoint sub to a revision never fetched locally")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	err := WorktreeInitSubmodules(ctx, caller, wt)
	if err == nil {
		t.Fatal("expected a clear failure for a submodule revision never fetched locally")
	}
}

func TestWorktreeInitSubmodulesIgnoresConfiguredUpdateNone(t *testing.T) {
	ctx := context.Background()
	caller, subSHA := submoduleFixture(t)
	run(t, caller, "git", "config", "-f", ".gitmodules", "submodule.sub.update", "none")
	run(t, caller, "git", "add", ".gitmodules")
	run(t, caller, "git", "commit", "-q", "-m", "configure submodule.sub.update=none")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules: %v", err)
	}
	if got := run(t, filepath.Join(wt, "sub"), "git", "rev-parse", "HEAD"); got != subSHA {
		t.Fatalf("submodule.<name>.update=none must not skip materialization; got %s, want %s", got, subSHA)
	}
}

// TestWorktreeInitSubmodulesMakesNoNetworkConnection is the listener-backed
// proof the design requires: even when .gitmodules names an address that
// would accept a connection, materializing an already-locally-available
// submodule revision contacts it zero times.
func TestWorktreeInitSubmodulesMakesNoNetworkConnection(t *testing.T) {
	ctx := context.Background()
	caller, subSHA := submoduleFixture(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	connections := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections++
			conn.Close()
		}
	}()

	// Retarget .gitmodules at the listener - a reachable "remote" that must
	// never actually be contacted - to prove the invariant is structural,
	// not just "the real remote happened to be unreachable".
	run(t, caller, "git", "config", "-f", ".gitmodules", "submodule.sub.url", "http://"+ln.Addr().String()+"/should-never-be-contacted")
	run(t, caller, "git", "add", ".gitmodules")
	run(t, caller, "git", "commit", "-q", "-m", "retarget submodule url at a live but untrusted listener")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules: %v", err)
	}
	if got := run(t, filepath.Join(wt, "sub"), "git", "rev-parse", "HEAD"); got != subSHA {
		t.Fatalf("materialized submodule at %s, want %s", got, subSHA)
	}

	ln.Close()
	<-done
	if connections != 0 {
		t.Fatalf("materialization contacted the listener %d time(s); must be zero", connections)
	}
}
