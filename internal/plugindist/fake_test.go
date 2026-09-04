package plugindist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests fake the whole network. Nothing here reaches GitHub, and a test
// that needed to would be testing GitHub.
type fakeGitHub struct {
	t      *testing.T
	owner  string
	repo   string
	tags   map[string]map[string][]byte // tag -> asset name -> bytes
	latest string
	server *httptest.Server
}

func newFakeGitHub(t *testing.T, owner, repo string) *fakeGitHub {
	t.Helper()

	fake := &fakeGitHub{t: t, owner: owner, repo: repo, tags: map[string]map[string][]byte{}}
	fake.server = httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

// publish adds a release and generates checksums.txt over its assets, which is
// what a release pipeline following the convention produces.
func (g *fakeGitHub) publish(tag string, assets map[string][]byte) {
	g.t.Helper()

	published := map[string][]byte{}
	for name, body := range assets {
		published[name] = body
	}
	published[ChecksumsAsset] = checksumsFor(published)

	g.tags[tag], g.latest = published, tag
}

// attach adds an asset to a published release without touching checksums.txt,
// which is how a signature is published beside the file it signs.
func (g *fakeGitHub) attach(tag, name string, body []byte) {
	g.t.Helper()

	release, published := g.tags[tag]
	if !published {
		g.t.Fatalf("no release published for %s", tag)
	}
	release[name] = body
}

// tamper replaces one asset's bytes and leaves checksums.txt alone: a mutated
// release asset, or a hostile mirror.
func (g *fakeGitHub) tamper(tag, name string, body []byte) {
	g.t.Helper()

	g.attach(tag, name, body)
}

func (g *fakeGitHub) asset(tag, name string) []byte {
	g.t.Helper()

	body, published := g.tags[tag][name]
	if !published {
		g.t.Fatalf("no asset %s published for %s", name, tag)
	}
	return body
}

func (g *fakeGitHub) installer(store *Store) *Installer {
	return &Installer{Store: store, HTTP: g.server.Client(), APIBase: g.server.URL}
}

func (g *fakeGitHub) downloadURL(tag, name string) string {
	return g.server.URL + "/download/" + tag + "/" + name
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	prefix := "/repos/" + g.owner + "/" + g.repo + "/releases/"

	switch {
	case r.URL.Path == prefix+"latest":
		g.writeRelease(w, g.latest)
	case strings.HasPrefix(r.URL.Path, prefix+"tags/"):
		g.writeRelease(w, strings.TrimPrefix(r.URL.Path, prefix+"tags/"))
	case strings.HasPrefix(r.URL.Path, "/download/"):
		segments := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/download/"), "/", 2)
		if len(segments) != 2 {
			http.NotFound(w, r)
			return
		}
		body, published := g.tags[segments[0]][segments[1]]
		if !published {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) writeRelease(w http.ResponseWriter, tag string) {
	assets, published := g.tags[tag]
	if !published {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}

	body := release{TagName: tag}
	for name := range assets {
		body.Assets = append(body.Assets, struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: name, URL: g.downloadURL(tag, name)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// archiveWith builds the tar.gz a goreleaser release publishes: the binary,
// executable, beside the files nobody runs.
func archiveWith(t *testing.T, binaryName string, body []byte) []byte {
	t.Helper()

	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)

	entries := []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "LICENSE", mode: 0o644, body: []byte("MIT")},
		{name: binaryName, mode: 0o755, body: body},
	}
	for _, entry := range entries {
		header := &tar.Header{
			Typeflag: tar.TypeReg, Name: entry.name,
			Mode: entry.mode, Size: int64(len(entry.body)),
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buffer.Bytes()
}

func checksumsFor(assets map[string][]byte) []byte {
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}

	var lines strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(assets[name])
		lines.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return []byte(lines.String())
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func lockPath(dir string) string {
	return filepath.Join(dir, LockFileName)
}
