package git

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// TestWorktreeInitSubmodulesRejectsTraversalInName proves a malicious
// .gitmodules section name cannot escape the local modules directory (the
// CVE-2018-11235 class of bug): the gitlink at "sub" stays real and
// resolvable, but the section that names it uses a traversal payload.
func TestWorktreeInitSubmodulesRejectsTraversalInName(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)

	writeFile(t, filepath.Join(caller, ".gitmodules"), "[submodule \"../../evil\"]\n\tpath = sub\n\turl = http://example.invalid/sub.git\n")
	run(t, caller, "git", "add", ".gitmodules")
	run(t, caller, "git", "commit", "-q", "-m", "malicious submodule name")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err == nil {
		t.Fatal("expected rejection of a traversal submodule name")
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "README.md")); err == nil {
		t.Fatal("a rejected traversal name must not materialize anything")
	}
}

// TestWorktreeInitSubmodulesRejectsTraversalInPath proves a malicious
// .gitmodules path is rejected before it is ever joined into a filesystem
// path, independent of whatever the committed tree happens to contain there.
func TestWorktreeInitSubmodulesRejectsTraversalInPath(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)

	writeFile(t, filepath.Join(caller, ".gitmodules"), "[submodule \"sub\"]\n\tpath = ../../escape\n\turl = http://example.invalid/sub.git\n")
	run(t, caller, "git", "add", ".gitmodules")
	run(t, caller, "git", "commit", "-q", "-m", "malicious submodule path")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err == nil {
		t.Fatal("expected rejection of a traversal submodule path")
	}
}

// TestWorktreeInitSubmodulesMalformedGitmodulesFailsClosed proves a
// committed .gitmodules with a gitlink section but no path key is a hard
// error, not a silent no-op that would let the pipeline run against an
// incomplete checkout.
func TestWorktreeInitSubmodulesMalformedGitmodulesFailsClosed(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)

	writeFile(t, filepath.Join(caller, ".gitmodules"), "[submodule \"sub\"]\n\turl = http://example.invalid/sub.git\n")
	run(t, caller, "git", "add", ".gitmodules")
	run(t, caller, "git", "commit", "-q", "-m", "malformed gitmodules missing path key")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err == nil {
		t.Fatal("expected an error for a committed .gitmodules with no valid path entries")
	}
}

// TestWorktreeInitSubmodulesRejectsUndeclaredGitlink proves a committed
// gitlink with no corresponding valid .gitmodules path entry is a hard
// error, even when other, well-formed submodule entries exist alongside it -
// a mixed valid/malformed .gitmodules must not let one entry silently
// shadow the omission of another.
func TestWorktreeInitSubmodulesRejectsUndeclaredGitlink(t *testing.T) {
	ctx := context.Background()
	caller, subSHA := submoduleFixture(t)

	run(t, caller, "git", "update-index", "--add", "--cacheinfo", "160000,"+subSHA+",sub-hidden")
	run(t, caller, "git", "commit", "-q", "-m", "commit an undeclared gitlink")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err == nil {
		t.Fatal("expected an error for a committed gitlink with no .gitmodules entry naming it")
	}
}

// TestWorktreeInitSubmodulesNeutralizesArbitraryGlobalFilter proves an
// operator-configured Git filter, selected by a pushed .gitattributes, does
// not run as a side effect of materialization - not just the well-known Git
// LFS filter names, but any filter name at all.
func TestWorktreeInitSubmodulesNeutralizesArbitraryGlobalFilter(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)

	marker := filepath.Join(t.TempDir(), "filter-fired")
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, globalConfig, "[filter \"marker\"]\n\tclean = cat\n\tsmudge = touch "+marker+" && cat\n\trequired = true\n")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	subPath := filepath.Join(caller, "sub")
	writeFile(t, filepath.Join(subPath, ".gitattributes"), "README.md filter=marker\n")
	run(t, subPath, "git", "add", ".gitattributes")
	run(t, subPath, "git", "commit", "-q", "-m", "select the marker filter for README.md")
	run(t, caller, "git", "add", "sub")
	run(t, caller, "git", "commit", "-q", "-m", "advance sub to the filter-selecting commit")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an operator-configured global filter fired during materialization; it must be neutralized")
	}
}

// TestWorktreeInitSubmodulesPrunesMaterializedSiblingsOnFailure proves a
// later sibling's failure does not leave an earlier sibling's worktree
// registration stranded in its embedded Git directory.
func TestWorktreeInitSubmodulesPrunesMaterializedSiblingsOnFailure(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)
	root := filepath.Dir(caller)

	sub2Bare := filepath.Join(root, "sub2.git")
	if err := InitBare(ctx, sub2Bare); err != nil {
		t.Fatal(err)
	}
	sub2Seed := initTestRepo(t)
	run(t, sub2Seed, "git", "remote", "add", "origin", sub2Bare)
	run(t, sub2Seed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	run(t, sub2Bare, "git", "symbolic-ref", "HEAD", "refs/heads/main")

	// Add sub2 at this original commit first, so the caller's embedded store
	// only ever fetches it - then advance sub2Bare's own remote separately,
	// so that new commit is never fetched into the caller.
	runAllow(t, caller, "submodule", "add", sub2Bare, "sub2")
	run(t, caller, "git", "commit", "-q", "-m", "add second submodule")

	sub2Seed2 := filepath.Join(root, "sub2-seed-2")
	runAllow(t, root, "clone", "-q", sub2Bare, sub2Seed2)
	run(t, sub2Seed2, "git", "config", "user.email", "test@test.com")
	run(t, sub2Seed2, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(sub2Seed2, "extra.txt"), "extra\n")
	run(t, sub2Seed2, "git", "add", ".")
	run(t, sub2Seed2, "git", "commit", "-q", "-m", "never fetched into caller")
	run(t, sub2Seed2, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	sub2NewSHA := run(t, sub2Seed2, "git", "rev-parse", "HEAD")

	run(t, caller, "git", "update-index", "--cacheinfo", "160000,"+sub2NewSHA+",sub2")
	run(t, caller, "git", "commit", "-q", "-m", "repoint sub2 to a commit never fetched locally")
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err == nil {
		t.Fatal("expected failure materializing sub2")
	}

	// "sub" sorts before "sub2", so it is guaranteed to have been
	// materialized before sub2's failure; its worktree registration in the
	// embedded Git directory must have been pruned again.
	subGitDir := filepath.Join(caller, ".git", "modules", "sub")
	listOut := run(t, subGitDir, "git", "worktree", "list", "--porcelain")
	if strings.Contains(listOut, filepath.Join(wt, "sub")) {
		t.Fatalf("materialized sibling submodule %q was not pruned after sub2 failed:\n%s", "sub", listOut)
	}
}

// TestWorktreeInitSubmodulesSuppressesPostCheckoutHook proves a
// post-checkout hook already present in a submodule's embedded Git
// directory does not run as a side effect of materialization.
func TestWorktreeInitSubmodulesSuppressesPostCheckoutHook(t *testing.T) {
	ctx := context.Background()
	caller, _ := submoduleFixture(t)
	callerSHA := run(t, caller, "git", "rev-parse", "HEAD")

	hookMarker := filepath.Join(t.TempDir(), "hook-fired")
	hooksDir := filepath.Join(caller, ".git", "modules", "sub", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(hooksDir, "post-checkout"), "#!/bin/sh\ntouch "+hookMarker+"\n")
	if err := os.Chmod(filepath.Join(hooksDir, "post-checkout"), 0o755); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := WorktreeAdd(ctx, caller, wt, callerSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := WorktreeInitSubmodules(ctx, caller, wt); err != nil {
		t.Fatalf("WorktreeInitSubmodules: %v", err)
	}
	if _, err := os.Stat(hookMarker); err == nil {
		t.Fatal("a post-checkout hook already present in the embedded Git directory fired during materialization; it must be suppressed")
	}
}
