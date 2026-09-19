package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPushReceivedSubmoduleInitFailureRemovesPartialWorktree covers a pushed
// commit whose .gitmodules names a submodule that was never initialized in
// the gate repository's own object store - the local-only design's fail-
// closed case, reached through the manager boundary rather than by calling
// git.WorktreeInitSubmodules directly. A plain `git push` of the parent
// repository never touches a submodule's own repository, so the gate bare
// repo this test builds genuinely has no local object store for it, with no
// extra effort needed to simulate that.
func TestPushReceivedSubmoduleInitFailureRemovesPartialWorktree(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})

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

	bareDir := p.RepoDir("unreachable-submodule-repo")
	gitCmd(t, "", "init", "--bare", bareDir)
	gitCmd(t, workDir, "remote", "add", "gate", bareDir)
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/main")
	gitCmd(t, bareDir, "remote", "add", "origin", bareDir)

	repo, err := d.InsertRepoWithID("unreachable-submodule-repo", workDir, "https://github.com/test/repo", "main")
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
		t.Fatal("run setup succeeded although the submodule was never initialized in the gate repository")
	}

	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("submodule init failure left %q behind in the operator's worktree root", filepath.Join(root, entry.Name()))
	}
}
