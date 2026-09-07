package plugindist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/setthasit/Lore/internal/config"
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

// urlArtifact is one artifact published at a URL, which has no release
// metadata to fake. It records the paths it was asked for, so a test can see a
// fetch that should not have happened.
type urlArtifact struct {
	coord     Coordinate
	installer *Installer

	mu        sync.Mutex
	artifact  []byte
	requested []string
}

func serveArtifact(t *testing.T, name, urlPath string, body []byte) *urlArtifact {
	t.Helper()

	served := &urlArtifact{artifact: body}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.record(r.URL.Path)
		if r.URL.Path != urlPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(served.body())
	}))
	t.Cleanup(server.Close)

	coord, err := Resolve(".", config.PluginDecl{Name: name, From: server.URL + urlPath})
	if err != nil {
		t.Fatalf("resolve %s: %v", urlPath, err)
	}
	store := NewStore(t.TempDir())

	served.coord, served.installer = coord, newInstaller(store, server.Client(), DefaultAPIBase())
	return served
}

func (a *urlArtifact) record(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.requested = append(a.requested, path)
}

func (a *urlArtifact) publish(body []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.artifact = body
}

func (a *urlArtifact) body() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.artifact
}

func (a *urlArtifact) asked(suffix string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	matched := []string(nil)
	for _, path := range a.requested {
		if strings.HasSuffix(path, suffix) {
			matched = append(matched, path)
		}
	}
	return matched
}

func (a *urlArtifact) install(t *testing.T, lock *Lock, pubkeyPath string) (Result, error) {
	t.Helper()

	coord := a.coord
	coord.PubKey = pubkeyPath
	return a.installer.Install(context.Background(), Request{Coordinate: coord}, lock)
}
