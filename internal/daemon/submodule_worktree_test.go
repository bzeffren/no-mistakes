package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// buildSubBare creates a bare submodule repository with one commit and
// returns its path.
func buildSubBare(t *testing.T) string {
	t.Helper()
	subBare := filepath.Join(t.TempDir(), "sub.git")
	gitCmd(t, "", "init", "--bare", subBare)
	subSeed := filepath.Join(t.TempDir(), "sub-seed")
	if err := os.MkdirAll(subSeed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, subSeed, "init")
	gitCmd(t, subSeed, "config", "user.email", "test@test.com")
	gitCmd(t, subSeed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(subSeed, "sub.txt"), []byte("sub"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, subSeed, "add", ".")
	gitCmd(t, subSeed, "commit", "-m", "initial")
	gitCmd(t, subSeed, "remote", "add", "origin", subBare)
	gitCmd(t, subSeed, "push", "origin", "HEAD:refs/heads/main")
	gitCmd(t, subBare, "symbolic-ref", "HEAD", "refs/heads/main")
	return subBare
}

// TestPushReceivedSubmoduleMaterializesFromWorkingCopyNotGate is the
// production topology: a bare gate populated purely by push (which never
// carries a submodule's own object store) alongside a separately registered
// working copy that genuinely has the submodule initialized, the way an
// operator's normal `git submodule update --init` leaves it. A run must
// succeed and its worktree must have the submodule materialized, proving
// materialization reads the working copy, not the gate.
func TestPushReceivedSubmoduleMaterializesFromWorkingCopyNotGate(t *testing.T) {
	step := &mockWorkDirStep{name: types.StepReview, workDir: make(chan string, 1)}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	subBare := buildSubBare(t)

	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("auto_fix:\n  lint: 0\n  test: 0\n  review: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "commit", "-m", "initial")
	gitCmd(t, workDir, "-c", "protocol.file.allow=always", "submodule", "add", subBare, "sub")
	gitCmd(t, workDir, "commit", "-m", "add submodule")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	// The bare gate: populated only by this push, exactly like production -
	// it never receives the submodule's own object store.
	bareDir := p.RepoDir("gate-vs-workingcopy-repo")
	gitCmd(t, "", "init", "--bare", bareDir)
	gitCmd(t, workDir, "remote", "add", "gate", bareDir)
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, bareDir, "remote", "add", "origin", bareDir)

	repo, err := d.InsertRepoWithID("gate-vs-workingcopy-repo", workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo-runs")
	configureWorktreeRoot(t, p, repo.WorkingPath, root)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: bareDir,
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatalf("run setup failed although the submodule is initialized in the registered working copy: %v", err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	var executedIn string
	select {
	case executedIn = <-step.workDir:
	default:
		t.Fatal("step never ran, so no worktree was observed")
	}
	if _, err := os.Stat(filepath.Join(executedIn, "sub", "sub.txt")); err != nil {
		t.Fatalf("submodule content was not materialized in the run worktree: %v", err)
	}
}

// TestPushReceivedSubmoduleInitFailureRemovesPartialWorktree covers a pushed
// commit whose .gitmodules names a submodule that was never initialized in
// the repository's own registered working copy - the local-only design's
// fail-closed case, reached through the manager boundary rather than by
// calling git.WorktreeInitSubmodules directly.
func TestPushReceivedSubmoduleInitFailureRemovesPartialWorktree(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

	subBare := buildSubBare(t)

	// A working copy that committed the gitlink via plumbing, without ever
	// running `git submodule add`/`update --init` - a real, if unusual,
	// state for an operator's own checkout to be in, and the state every
	// checkout starts in before its first submodule update.
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("auto_fix:\n  lint: 0\n  test: 0\n  review: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".gitmodules"), []byte("[submodule \"sub\"]\n\tpath = sub\n\turl = "+subBare+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subSHA := gitOutput(t, subBare, "rev-parse", "refs/heads/main")
	gitCmd(t, workDir, "add", ".")
	gitCmd(t, workDir, "update-index", "--add", "--cacheinfo", "160000,"+subSHA+",sub")
	gitCmd(t, workDir, "commit", "-m", "add submodule gitlink without initializing it locally")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")

	bareDir := p.RepoDir("uninitialized-submodule-repo")
	gitCmd(t, "", "init", "--bare", bareDir)
	gitCmd(t, workDir, "remote", "add", "gate", bareDir)
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, bareDir, "remote", "add", "origin", bareDir)

	repo, err := d.InsertRepoWithID("uninitialized-submodule-repo", workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo-runs")
	configureWorktreeRoot(t, p, repo.WorkingPath, root)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	callErr := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: bareDir,
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if callErr == nil {
		t.Fatal("run setup succeeded although the submodule was never initialized in the registered working copy")
	}
	if !strings.Contains(callErr.Error(), "initialize or fetch it in the repository's normal trusted working copy first") {
		t.Fatalf("error lacks the actionable diagnostic AGENTS.md promises: %v", callErr)
	}

	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("submodule init failure left %q behind in the operator's worktree root", filepath.Join(root, entry.Name()))
	}
}
