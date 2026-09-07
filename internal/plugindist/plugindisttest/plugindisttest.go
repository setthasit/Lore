// Package plugindisttest hosts the fake GitHub release server shared by the plugindist and CLI tests.
package plugindisttest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

const checksumsAsset = "checksums.txt"

type GitHub struct {
	*httptest.Server

	t      *testing.T
	repo   string
	prefix string
	tags   map[string]map[string][]byte
	latest string
}

// NewGitHub serves over TLS: reach it with Client(), or trust Certificate() where the code under test finds roots.
func NewGitHub(t *testing.T, owner, repo string) *GitHub {
	t.Helper()

	fake := &GitHub{
		t:      t,
		repo:   repo,
		prefix: "/repos/" + owner + "/" + repo + "/releases/",
		tags:   map[string]map[string][]byte{},
	}
	fake.Server = httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

// Publish also generates checksums.txt over the assets.
func (g *GitHub) Publish(tag string, assets map[string][]byte) {
	g.t.Helper()

	published := map[string][]byte{}
	for name, body := range assets {
		published[name] = body
	}
	published[checksumsAsset] = checksums(published)

	g.tags[tag], g.latest = published, tag
}

// Attach writes one asset and leaves checksums.txt as published.
func (g *GitHub) Attach(tag, name string, body []byte) {
	g.t.Helper()

	release, published := g.tags[tag]
	if !published {
		g.t.Fatalf("no release published for %s", tag)
	}
	release[name] = body
}

func (g *GitHub) Asset(tag, name string) []byte {
	g.t.Helper()

	body, published := g.tags[tag][name]
	if !published {
		g.t.Fatalf("no asset %s published for %s", name, tag)
	}
	return body
}

func (g *GitHub) DownloadURL(tag, name string) string {
	return g.URL + "/download/" + tag + "/" + name
}

func (g *GitHub) AssetName(tag string) string {
	return g.repo + "_" + strings.TrimPrefix(tag, "v") + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
}

func (g *GitHub) Digest(tag, name string) string {
	sum := sha256.Sum256(g.Asset(tag, name))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (g *GitHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == g.prefix+"latest":
		g.writeRelease(w, g.latest)
	case strings.HasPrefix(r.URL.Path, g.prefix+"tags/"):
		g.writeRelease(w, strings.TrimPrefix(r.URL.Path, g.prefix+"tags/"))
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

func (g *GitHub) writeRelease(w http.ResponseWriter, tag string) {
	assets, published := g.tags[tag]
	if !published {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}

	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	body := struct {
		TagName string  `json:"tag_name"`
		Assets  []asset `json:"assets"`
	}{TagName: tag}
	for name := range assets {
		body.Assets = append(body.Assets, asset{Name: name, URL: g.DownloadURL(tag, name)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func Archive(t *testing.T, binaryName string, body []byte) []byte {
	t.Helper()

	return TarGz(t,
		TarEntry{Name: "LICENSE", Mode: 0o644, Body: []byte("MIT")},
		TarEntry{Name: binaryName, Mode: 0o755, Body: body},
	)
}

type TarEntry struct {
	Name string
	Mode int64
	Body []byte
}

func TarGz(t *testing.T, entries ...TarEntry) []byte {
	t.Helper()

	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)

	for _, entry := range entries {
		header := &tar.Header{
			Typeflag: tar.TypeReg, Name: entry.Name,
			Mode: entry.Mode, Size: int64(len(entry.Body)),
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := writer.Write(entry.Body); err != nil {
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

func checksums(assets map[string][]byte) []byte {
	var lines strings.Builder
	for name, body := range assets {
		sum := sha256.Sum256(body)
		lines.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return []byte(lines.String())
}
