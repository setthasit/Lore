package fsx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/setthasit/Lore/internal/fsx"
)

func TestWriteAtomicCreatesAFileThatDoesNotExistYet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lore.lock")

	if err := fsx.WriteAtomic(path, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "version: 1\n" {
		t.Errorf("content = %q, want %q", body, "version: 1\n")
	}
	assertOnly(t, dir, "lore.lock")
}

func TestWriteAtomicAppliesTheRequestedMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []os.FileMode{0o644, 0o755, 0o600} {
		path := filepath.Join(t.TempDir(), "artifact")
		if err := fsx.WriteAtomic(path, []byte("body"), mode); err != nil {
			t.Fatalf("write %v: %v", mode, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %v: %v", mode, err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("mode = %v, want %v", got, mode)
		}
	}
}

func TestWriteAtomicReplacesTheWholeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lore.lock")
	if err := os.WriteFile(path, []byte("a much longer previous file\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := fsx.WriteAtomic(path, []byte("short\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "short\n" {
		t.Errorf("content = %q, want the new file only", body)
	}
	assertOnly(t, dir, "lore.lock")
}

func TestWriteAtomicRemovesTheStagedFileWhenTheRenameFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lore.lock")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed a directory in the way: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("kept"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := fsx.WriteAtomic(path, []byte("version: 1\n"), 0o644); err == nil {
		t.Fatal("renaming onto a non-empty directory succeeded, want a failure")
	}

	body, err := os.ReadFile(filepath.Join(path, "occupant"))
	if err != nil || string(body) != "kept" {
		t.Errorf("the destination was disturbed: content = %q, err = %v", body, err)
	}
	assertOnly(t, dir, "lore.lock")
}

func TestWriteAtomicKeepsThePreviousContentWhenTheDirectoryRefusesTheWrite(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "lore.lock")
	if err := os.WriteFile(path, []byte("previous\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("seal the directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := fsx.WriteAtomic(path, []byte("replacement\n"), 0o644); err == nil {
		t.Fatal("writing into a read-only directory succeeded, want a failure")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "previous\n" {
		t.Errorf("content = %q, want the previous file", body)
	}
	assertOnly(t, dir, "lore.lock")
}

func assertOnly(t *testing.T, dir string, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("%s holds %v, want exactly %v", dir, got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("%s holds %v, want exactly %v", dir, got, want)
		}
	}
}
