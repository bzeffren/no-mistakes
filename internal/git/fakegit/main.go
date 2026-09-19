// Command fakegit is the tiny cross-platform fake git that internal/git tests
// place on PATH when they must stall or block one specific git invocation. It
// forwards every command to the real git binary named in NM_FAKE_GIT_REAL and
// only diverges for the command the test selects, so a stall or a
// killed-after-registration worktree add can be exercised deterministically on
// every platform (a /bin/sh shim would not be resolved via PATHEXT on Windows).
// It is compiled once per test process and linked onto PATH as git, following
// internal/pipeline/fakecli.
package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	real := os.Getenv("NM_FAKE_GIT_REAL")
	args := os.Args[1:]

	switch os.Getenv("NM_FAKE_GIT_MODE") {
	case "stall":
		if match := os.Getenv("NM_FAKE_GIT_MATCH"); match != "" && anyArgContains(args, match) {
			blockUntilKilled()
		}
	case "register-block":
		if isWorktreeAdd(args) {
			// Let the worktree genuinely register, but signal readiness only
			// once it did: a failed add must surface to the test as that
			// failing exit code, not masquerade as a clean cancellation with
			// nothing left to clean up.
			if code := forwardToRealGit(real, args); code != 0 {
				os.Exit(code)
			}
			if ready := os.Getenv("NM_FAKE_GIT_READY"); ready != "" {
				_ = os.WriteFile(ready, []byte("ready"), 0o600)
			}
			blockUntilKilled()
		}
	}
	os.Exit(forwardToRealGit(real, args))
}

// forwardToRealGit runs real git with this process's own arguments, standard
// streams, environment, and working directory, returning its exit code. The
// environment (including NM_FAKE_GIT_*) is inherited unchanged, so any nested
// git real git happens to spawn simply passes through this fake again.
func forwardToRealGit(real string, args []string) int {
	if real == "" {
		os.Stderr.WriteString("fakegit: NM_FAKE_GIT_REAL is unset\n")
		return 127
	}
	cmd := exec.Command(real, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

func anyArgContains(args []string, substr string) bool {
	for _, a := range args {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

func isWorktreeAdd(args []string) bool {
	var worktree, add bool
	for _, a := range args {
		switch a {
		case "worktree":
			worktree = true
		case "add":
			add = true
		}
	}
	return worktree && add
}

// blockUntilKilled parks until the parent's context cancellation kills the
// process (group). A long sleep, not select{}, keeps a timer pending so the Go
// runtime does not report a deadlock.
func blockUntilKilled() {
	time.Sleep(time.Hour)
	os.Exit(1)
}
