package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/terva-sh/zot-web/internal/version"
)

func TestSaveToWorkspaceWritesUnderCWD(t *testing.T) {
	cwd := t.TempDir()
	rel, err := saveToWorkspace(cwd, "assets/logo.png", []byte("png"), false)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rel != filepath.Join("assets", "logo.png") {
		t.Errorf("rel = %q", rel)
	}
	got, err := os.ReadFile(filepath.Join(cwd, "assets", "logo.png"))
	if err != nil || string(got) != "png" {
		t.Errorf("file content = %q, err = %v", got, err)
	}
}

func TestSaveToWorkspaceRejectsEscape(t *testing.T) {
	cwd := t.TempDir()
	for _, p := range []string{"../escape.png", "a/../../escape.png", "/etc/passwd"} {
		if _, err := saveToWorkspace(cwd, p, []byte("x"), false); err == nil {
			t.Errorf("save_path %q should have been rejected", p)
		}
	}
	// Nothing should have been written outside cwd.
	if _, err := os.Stat(filepath.Join(filepath.Dir(cwd), "escape.png")); err == nil {
		t.Error("a file escaped the workspace")
	}
}

func TestSaveToWorkspaceOverwritePolicy(t *testing.T) {
	cwd := t.TempDir()
	if _, err := saveToWorkspace(cwd, "f.png", []byte("v1"), false); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := saveToWorkspace(cwd, "f.png", []byte("v2"), false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected no-clobber error, got %v", err)
	}
	if _, err := saveToWorkspace(cwd, "f.png", []byte("v2"), true); err != nil {
		t.Fatalf("overwrite save: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, "f.png"))
	if string(got) != "v2" {
		t.Errorf("after overwrite content = %q, want v2", got)
	}
}

func TestSaveToWorkspaceNoCWD(t *testing.T) {
	if _, err := saveToWorkspace("", "f.png", []byte("x"), false); err == nil {
		t.Error("empty cwd should be rejected")
	}
}

func TestSaveToWorkspaceRejectsGitDir(t *testing.T) {
	cwd := t.TempDir()
	// .git/ itself
	if _, err := saveToWorkspace(cwd, ".git", []byte("x"), false); err == nil {
		t.Error("should reject bare .git path")
	}
	// .git/config
	if _, err := saveToWorkspace(cwd, ".git/config", []byte("x"), false); err == nil {
		t.Error("should reject .git/config")
	}
	// .git/hooks/some-hook
	if _, err := saveToWorkspace(cwd, ".git/hooks/pre-commit", []byte("x"), false); err == nil {
		t.Error("should reject files under .git/")
	}
	// But .gitignore or .gitattributes should still work
	if _, err := saveToWorkspace(cwd, ".gitignore", []byte("x"), false); err != nil {
		t.Errorf(".gitignore should be allowed, got: %v", err)
	}
}

func TestSaveToWorkspaceRejectsSymlinkParentEscape(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := saveToWorkspace(cwd, filepath.Join("out", "file.txt"), []byte("x"), false); err == nil {
		t.Fatal("symlinked parent should be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); err == nil {
		t.Fatal("file was written through symlink outside workspace")
	}
}

func TestSaveToWorkspaceRejectsFinalSymlink(t *testing.T) {
	cwd := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := saveToWorkspace(cwd, "link.txt", []byte("replace"), true); err == nil {
		t.Fatal("final symlink should be rejected even with overwrite=true")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("symlink target was modified: %q", got)
	}
}

func TestVersionString(t *testing.T) {
	got := versionString()
	if !strings.HasPrefix(got, "zot-web "+version.Version) {
		t.Errorf("versionString() = %q, want prefix %q", got, "zot-web "+version.Version)
	}
	if !strings.Contains(got, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("versionString() = %q, missing platform", got)
	}
}
