package plugindist

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// RootEnv overrides where installed plugins live. It exists so a machine that
// keeps its state somewhere other than the home directory can say so, and so a
// test never writes to a real home.
const RootEnv = "LORE_HOME"

const (
	// digestFileName holds the digest of the unpacked binary and is re-checked
	// at every launch, so a cached binary rewritten after installation is
	// caught too — not only a mutated download.
	digestFileName = ".digest"

	// manifestFileName caches what the `manifest` handshake returned. It is a
	// cache and never an authority: deleting it costs one exec, and a stale
	// copy can never outvote the binary, which is re-asked every launch.
	manifestFileName = "manifest.json"

	pluginsDirName = "plugins"
)

// Store is the on-disk cache of installed plugins. Versions live in separate
// directories so several may coexist on one machine — different workspaces pin
// differently — and the workspace's lockfile decides which one runs. The cache
// itself never picks.
type Store struct {
	root     string
	platform Platform
}

// NewStore roots a cache at an explicit directory.
func NewStore(root string) *Store {
	return &Store{root: root, platform: HostPlatform()}
}

// DefaultStore roots the cache where the rest of Lore keeps its state.
func DefaultStore() (*Store, error) {
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	return NewStore(root), nil
}

func DefaultRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv(RootEnv)); root != "" {
		return root, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", internalerror.NewPreconditionError("this user has no home directory, so there is nowhere to"+
			" install plugins — set "+RootEnv, err)
	}
	return filepath.Join(home, ".lore"), nil
}

// WithPlatform returns a store that reads and writes another platform's slot.
// Installing for a platform other than the running one is not a supported
// workflow; this exists so the platform-dependent paths are testable.
func (s *Store) WithPlatform(p Platform) *Store {
	clone := *s
	clone.platform = p
	return &clone
}

func (s *Store) Platform() Platform {
	return s.platform
}

func (s *Store) Root() string {
	return s.root
}

// Dir is where one version of one plugin lives.
func (s *Store) Dir(name, version string) string {
	return filepath.Join(s.root, pluginsDirName, name, version)
}

// ManifestPath is where the caller caches the manifest the handshake returned.
// This package never performs the handshake: it does not speak the protocol,
// and the manifest is always read from the binary rather than from a file in
// the archive, so a plugin cannot ship a manifest that disagrees with itself.
func (s *Store) ManifestPath(name, version string) string {
	return filepath.Join(s.Dir(name, version), manifestFileName)
}

// WriteManifest caches the handshake's answer beside the binary.
func (s *Store) WriteManifest(name, version string, raw []byte) error {
	path := s.ManifestPath(name, version)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return internalerror.NewInternalError("cannot cache the manifest at "+path, err)
	}
	return nil
}

// Report is what is known about an installed plugin: the binary that will run,
// the digest re-verified to reach it, and what the lockfile pinned. It is what
// `lore plugin verify` prints and what the host hands to the protocol layer.
type Report struct {
	Name     string
	Origin   Origin
	Platform string
	Version  string
	Binary   string

	LockedURL    string // empty for a local coordinate, which is not locked
	LockedDigest string // the artifact digest lore.lock pins
	BinaryDigest string // the re-verified digest of the binary on disk

	Manifest bool   // a manifest.json is cached beside the binary
	Warning  string // what the host must say at startup, empty when there is nothing to say
}

// Binary is the installed, digest-checked binary for a declared plugin, or an
// error naming the exact command that fixes it. Nothing here downloads: a
// declared-but-uninstalled plugin is a startup error, never a silent fetch.
func Binary(name string, coord Coordinate, lock *Lock) (string, error) {
	store, err := DefaultStore()
	if err != nil {
		return "", err
	}
	return store.Binary(name, coord, lock)
}

func (s *Store) Binary(name string, coord Coordinate, lock *Lock) (string, error) {
	report, err := s.Locate(name, coord, lock)
	if err != nil {
		return "", err
	}
	return report.Binary, nil
}

// Locate resolves a declaration to the binary that will run and re-verifies it.
// A digest mismatch refuses: it never warns and continues, and no flag makes it
// continue, because the thing being launched is code from someone else.
func (s *Store) Locate(name string, coord Coordinate, lock *Lock) (Report, error) {
	if name == "" {
		name = coord.Name
	}
	report := Report{Name: name, Origin: coord.Origin, Platform: s.platform.Key(), Warning: coord.Warning()}

	if coord.Origin == OriginLocal {
		info, err := os.Stat(coord.Path)
		if err != nil || info.IsDir() {
			return Report{}, internalerror.NewPreconditionError(label(name)+" runs "+coord.Path+
				" in place, but there is no file there", err)
		}
		report.Binary, report.Version = coord.Path, OriginLocal.String()
		return report, nil
	}

	artifact, locked := lock.Artifact(name, s.platform)
	if !locked {
		return Report{}, internalerror.NewPreconditionError(label(name)+" has no "+LockFileName+" entry for "+
			s.platform.Key()+" — run: lore plugin install "+name, nil)
	}
	entry, _ := lock.Entry(name)

	dir := s.Dir(name, entry.Version)
	binary, err := s.binaryIn(dir)
	if err != nil {
		return Report{}, err
	}
	if binary == "" {
		return Report{}, notInstalled(name)
	}

	recorded, err := os.ReadFile(filepath.Join(dir, digestFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Report{}, notInstalled(name)
		}
		return Report{}, internalerror.NewInternalError("cannot read the recorded digest of "+binary, err)
	}

	actual, err := digestFile(binary)
	if err != nil {
		return Report{}, err
	}
	if expected := strings.TrimSpace(string(recorded)); expected != actual {
		return Report{}, internalerror.NewPreconditionError(label(name)+": digest mismatch for "+s.platform.Key()+
			" (expected "+expected+", got "+actual+")", nil)
	}

	report.Version, report.Binary, report.BinaryDigest = entry.Version, binary, actual
	report.LockedURL, report.LockedDigest = artifact.URL, artifact.Digest
	if _, err := os.Stat(s.ManifestPath(name, entry.Version)); err == nil {
		report.Manifest = true
	}
	return report, nil
}

// binaryIn finds the executable in a version directory. The binary is not named
// in the lockfile because the archive names it, so the directory's own contents
// are the record: one file that is neither the digest nor the cached manifest.
func (s *Store) binaryIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", internalerror.NewInternalError("cannot read the plugin cache at "+dir, err)
	}

	found := ""
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == manifestFileName || strings.HasPrefix(name, ".") {
			continue
		}
		if found != "" {
			return "", internalerror.NewPreconditionError("the plugin cache at "+dir+" holds more than one binary"+
				" ("+found+" and "+name+"): delete the directory and install again", nil)
		}
		found = name
	}
	if found == "" {
		return "", nil
	}
	return filepath.Join(dir, found), nil
}

// write stores an unpacked binary and the digest re-checked at every launch.
// The binary lands under a temporary name and is renamed, so an interrupted
// write can never leave a half-written binary that a digest file vouches for.
func (s *Store) write(name, version, binaryName string, body []byte) (path, digest string, err error) {
	dir := s.Dir(name, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", internalerror.NewInternalError("cannot create the plugin cache at "+dir, err)
	}

	path = filepath.Join(dir, binaryName)
	staged := path + ".partial"
	if err := os.WriteFile(staged, body, 0o755); err != nil {
		return "", "", internalerror.NewInternalError("cannot write "+path, err)
	}
	if err := os.Rename(staged, path); err != nil {
		_ = os.Remove(staged)
		return "", "", internalerror.NewInternalError("cannot write "+path, err)
	}

	digest = digestOf(body)
	if err := os.WriteFile(filepath.Join(dir, digestFileName), []byte(digest+"\n"), 0o644); err != nil {
		// Without the digest file the binary can never be launched, so the
		// half-installed version is removed rather than left to fail later.
		_ = os.RemoveAll(dir)
		return "", "", internalerror.NewInternalError("cannot record the digest of "+path, err)
	}
	return path, digest, nil
}

// Remove deletes every cached version of a plugin and reports how many there
// were, which is what `lore plugin remove` tells the user it did.
func (s *Store) Remove(name string) (int, error) {
	dir := filepath.Join(s.root, pluginsDirName, name)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, internalerror.NewInternalError("cannot read the plugin cache at "+dir, err)
	}

	versions := 0
	for _, entry := range entries {
		if entry.IsDir() {
			versions++
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, internalerror.NewInternalError("cannot remove the plugin cache at "+dir, err)
	}
	return versions, nil
}

func notInstalled(name string) error {
	return internalerror.NewPreconditionError(label(name)+" is not installed — run: lore plugin install "+name, nil)
}

// A digest carries its algorithm, so a lockfile written today stays readable
// when a second algorithm exists.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// digestFile streams the file rather than reading it whole: this runs at every
// startup, for a binary that may be tens of megabytes.
func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", internalerror.NewInternalError("cannot read "+path, err)
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", internalerror.NewInternalError("cannot read "+path, err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
