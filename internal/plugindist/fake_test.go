package plugindist

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/setthasit/Lore/internal/plugindist/plugindisttest"
)

func fakeInstaller(fake *plugindisttest.GitHub, store *Store) *Installer {
	return newInstaller(store, fake.Client(), fake.URL)
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func cacheDir(t *testing.T, store *Store, name, version string) string {
	t.Helper()

	dir, err := store.Dir(name, version)
	if err != nil {
		t.Fatalf("cache directory for %s@%s: %v", name, version, err)
	}
	return dir
}

func lockPath(dir string) string {
	return filepath.Join(dir, LockFileName)
}
