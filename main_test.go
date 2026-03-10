package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFolders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr string
	}{
		{name: "multiple folders", input: "/tmp/a, /tmp/b", want: []string{"/tmp/a", "/tmp/b"}},
		{name: "requires absolute paths", input: "notes", wantErr: "folder path must be absolute"},
		{name: "requires at least one folder", input: " , ", wantErr: "at least one folder is required"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseFolders(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFolders returned error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("unexpected folder count: got %d want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("unexpected folder at index %d: got %q want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "valid config", cfg: Config{Password: "secret", GitURL: "git@github.com:test/repo.git", Folders: []string{"/tmp/a"}}},
		{name: "missing password", cfg: Config{GitURL: "git@github.com:test/repo.git", Folders: []string{"/tmp/a"}}, wantErr: "config password is required"},
		{name: "relative folder", cfg: Config{Password: "secret", GitURL: "git@github.com:test/repo.git", Folders: []string{"notes"}}, wantErr: "config folder path must be absolute"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateConfig(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateConfig returned error: %v", err)
			}
		})
	}
}

func TestRootNamesAllowDuplicateBasenames(t *testing.T) {
	t.Parallel()

	got, err := rootNamesForFolders([]string{
		"/Users/user1/one/notes",
		"/Users/user1/two/notes",
	})
	if err != nil {
		t.Fatalf("rootNamesForFolders returned error: %v", err)
	}
	if got[0] == got[1] {
		t.Fatalf("expected distinct root names, got %q", got[0])
	}
	if got[0] != "Users__user1__one__notes" || got[1] != "Users__user1__two__notes" {
		t.Fatalf("unexpected root name format: %q %q", got[0], got[1])
	}
}

func TestParseNameStatus(t *testing.T) {
	t.Parallel()

	changes, err := parseNameStatus("A\tone/file.txt\nM\ttwo/file.txt\nD\tthree/file.txt\n")
	if err != nil {
		t.Fatalf("parseNameStatus returned error: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("unexpected change count: %d", len(changes))
	}
	if changes[0].Status != "A" || changes[2].Status != "D" {
		t.Fatalf("unexpected parsed statuses: %+v", changes)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "plain.txt")
	encrypted := filepath.Join(dir, "plain.txt.enc")
	if err := os.WriteFile(source, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := encryptFile("secret", source, encrypted); err != nil {
		t.Fatalf("encryptFile returned error: %v", err)
	}
	got, err := decryptFile("secret", encrypted)
	if err != nil {
		t.Fatalf("decryptFile returned error: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("unexpected plaintext: %q", got)
	}
}

func TestSyncMirrorRemovesDeletedFiles(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	mirror := t.TempDir()
	file := filepath.Join(source, "note.txt")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := syncMirror(mirror, []string{source}); err != nil {
		t.Fatalf("syncMirror first run: %v", err)
	}
	root, err := rootName(source)
	if err != nil {
		t.Fatalf("rootName returned error: %v", err)
	}
	mirrorFile := filepath.Join(mirror, root, "note.txt")
	if _, err := os.Stat(mirrorFile); err != nil {
		t.Fatalf("expected mirrored file: %v", err)
	}

	if err := os.Remove(file); err != nil {
		t.Fatalf("remove source file: %v", err)
	}
	if err := syncMirror(mirror, []string{source}); err != nil {
		t.Fatalf("syncMirror second run: %v", err)
	}
	if _, err := os.Stat(mirrorFile); !os.IsNotExist(err) {
		t.Fatalf("expected mirrored file to be removed, got %v", err)
	}
}

func TestSyncMirrorKeepsRootsRemovedFromConfig(t *testing.T) {
	t.Parallel()

	first := t.TempDir()
	second := t.TempDir()
	mirror := t.TempDir()

	if err := os.WriteFile(filepath.Join(first, "one.txt"), []byte("one"), 0o644); err != nil {
		t.Fatalf("write first source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "two.txt"), []byte("two"), 0o644); err != nil {
		t.Fatalf("write second source: %v", err)
	}

	if err := syncMirror(mirror, []string{first, second}); err != nil {
		t.Fatalf("syncMirror initial run: %v", err)
	}

	secondRoot, err := rootName(second)
	if err != nil {
		t.Fatalf("rootName returned error: %v", err)
	}
	secondMirrorFile := filepath.Join(mirror, secondRoot, "two.txt")
	if _, err := os.Stat(secondMirrorFile); err != nil {
		t.Fatalf("expected second root file to exist: %v", err)
	}

	if err := syncMirror(mirror, []string{first}); err != nil {
		t.Fatalf("syncMirror reduced config run: %v", err)
	}
	if _, err := os.Stat(secondMirrorFile); err != nil {
		t.Fatalf("expected removed-from-config root to be preserved, got %v", err)
	}
}

func TestPruneEmptyDirsRemovesEmptyDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	keepDir := filepath.Join(root, "keep")
	removeDir := filepath.Join(root, "remove", "nested")
	gitDir := filepath.Join(root, ".git", "objects")

	for _, dir := range []string{keepDir, removeDir, gitDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(keepDir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write keep file: %v", err)
	}

	if err := pruneEmptyDirs(root); err != nil {
		t.Fatalf("pruneEmptyDirs returned error: %v", err)
	}

	if _, err := os.Stat(removeDir); !os.IsNotExist(err) {
		t.Fatalf("expected empty directory to be removed, got %v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Fatalf("expected non-empty directory to remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Fatalf("expected .git directory to remain: %v", err)
	}
}
