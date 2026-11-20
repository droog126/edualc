package main

import (
	"path/filepath"
	"testing"
)

func TestPathMappingEnvAndGitLockClassification(t *testing.T) {
	mounts := []pathMount{
		{Guest: "/data/desktop/workspace/myapp", Host: "/Users/dev/myapp"},
		{Guest: "/workspace/myapp", Host: "/Users/dev/myapp"},
		{Guest: "/data/desktop", Host: "/Users/dev/edualc/data/desktop"},
		{Guest: "/tmp/edualc", Host: "/Users/dev/edualc/data/shared-tmp"},
	}

	got, err := mapGuestToHostCwd("/workspace/myapp/crates/core", mounts, "/Users/dev/myapp")
	if err != nil || got != filepath.Join("/Users/dev/myapp", "crates", "core") {
		t.Fatalf("unexpected workspace cwd: %q err=%v", got, err)
	}

	wt, err := mapGuestToHostCwd("/data/desktop/.claude/worktrees/w1", mounts, "/Users/dev/myapp")
	if err != nil || wt != filepath.Join("/Users/dev/edualc/data/desktop", ".claude", "worktrees", "w1") {
		t.Fatalf("unexpected worktree cwd: %q err=%v", wt, err)
	}

	if _, err := mapGuestToHostCwd("/workspace/myapp/../../etc", mounts, "/Users/dev/myapp"); err == nil {
		t.Fatalf("expected traversal rejection")
	}

	args := rewriteArgsGuestToHost([]string{"--manifest-path", "/workspace/myapp/Cargo.toml", "/tmp/edualc/run.py"}, mounts)
	if args[1] != "/Users/dev/myapp/Cargo.toml" || args[2] != "/Users/dev/edualc/data/shared-tmp/run.py" {
		t.Fatalf("unexpected rewritten args: %v", args)
	}

	scrubbed := scrubHostToGuest("error: /Users/dev/myapp/src/main.rs:12", mounts)
	if scrubbed != "error: /workspace/myapp/src/main.rs:12" {
		t.Fatalf("unexpected scrubbed output: %q", scrubbed)
	}

	if isGitWriteCommand("git", []string{"-c", "core.fsmonitor=", "status"}) {
		t.Fatalf("git status should be read-only")
	}
	if !isGitWriteCommand("git", []string{"commit", "-m", "hi"}) {
		t.Fatalf("git commit should be serialized write")
	}
}
