package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/terva-sh/zot-web/internal/proto"
	"github.com/terva-sh/zot-web/internal/version"
)

// TestManifestVersionMatchesCode pins extension.json's version (what the host
// shows in `ext list`) equal to internal/version.Version (the hello frame, the
// User-Agent, --version). They are bumped together at release; this guard fails
// the build if they drift, which is how they silently disagreed before.
func TestManifestVersionMatchesCode(t *testing.T) {
	b, err := os.ReadFile("extension.json")
	if err != nil {
		t.Fatalf("read extension.json: %v", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse extension.json: %v", err)
	}
	if m.Version != version.Version {
		t.Errorf("extension.json version %q != internal/version.Version %q — bump them together", m.Version, version.Version)
	}
}

// TestNetworkToolsDeclareAuthority guards that every tool zot-web registers
// declares network-read authority. They all reach the network, so terva must
// gate them; a tool added without proto.NetworkRead() would silently be treated
// as side-effecting/auto-allowable. This catches that the moment a new tool is
// added — register() is the same wiring main() runs.
func TestNetworkToolsDeclareAuthority(t *testing.T) {
	e := proto.New("web", "test")
	register(e)

	tools := e.Tools()
	if len(tools) == 0 {
		t.Fatal("register() declared no tools")
	}
	// Every web tool reaches the network; none may be unmarked.
	for _, ti := range tools {
		if ti.Authority != "network-read" {
			t.Errorf("tool %q authority = %q, want network-read (all web tools reach the network)", ti.Name, ti.Authority)
		}
	}
	// And the known set is present (catches an accidental drop / rename).
	want := []string{"web_search", "web_fetch", "web_images", "web_links", "web_fetch_raw", "web_fetch_image"}
	have := map[string]bool{}
	for _, ti := range tools {
		have[ti.Name] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}

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

func TestCheckSavePathMatchesSavePolicy(t *testing.T) {
	cwd := t.TempDir()
	for _, p := range []string{"../escape.png", "a/../../escape.png", "/etc/passwd", ".git/config"} {
		if err := checkSavePath(cwd, p, false); err == nil {
			t.Errorf("preflight should reject %q", p)
		}
	}
	if err := os.WriteFile(filepath.Join(cwd, "f.png"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkSavePath(cwd, "f.png", false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("preflight no-clobber: err = %v", err)
	}
	if err := checkSavePath(cwd, "f.png", true); err != nil {
		t.Errorf("preflight with overwrite should pass: %v", err)
	}
	if err := checkSavePath(cwd, "new/dir/f.png", false); err != nil {
		t.Errorf("preflight of a fresh nested path should pass: %v", err)
	}
}

func TestCheckSavePathHasNoSideEffects(t *testing.T) {
	cwd := t.TempDir()
	if err := checkSavePath(cwd, "a/b/c.png", false); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "a")); !os.IsNotExist(err) {
		t.Error("preflight created parent directories")
	}
}

func TestCheckSavePathRejectsSymlinks(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := checkSavePath(cwd, "out/file.txt", false); err == nil {
		t.Error("preflight should reject a symlinked parent")
	}
	if err := os.Symlink(filepath.Join(outside, "t.txt"), filepath.Join(cwd, "link.txt")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := checkSavePath(cwd, "link.txt", true); err == nil {
		t.Error("preflight should reject a symlink target")
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
