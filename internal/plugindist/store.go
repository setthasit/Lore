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
	"github.com/setthasit/Lore/internal/fsx"
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
	return &Store{root: root, platform: hostPlatform()}
}

// DefaultStore roots the cache where the rest of Lore keeps its state.
func DefaultStore() (*Store, error) {
	root, err := defaultRoot()
	if err != nil {
		return nil, err
	}
	return NewStore(root), nil
}

func defaultRoot() (string, error) {
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

func (s *Store) Dir(name, version string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	if !isCacheEntryName(version) {
		return "", internalerror.NewBadRequestError(Label(name)+" is pinned to "+version+", which is not a"+
			" usable version: a version is one directory in the plugin cache, so it must be a single name"+
			" that neither starts with a dot nor contains a path separator", nil)
	}
	return filepath.Join(s.root, pluginsDirName, name, version), nil
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

	Warning string // what the host must say at startup, empty when there is nothing to say
}

// Binary is the installed, digest-checked binary for a declared plugin, or an
// error naming the exact command that fixes it. Nothing here downloads: a
// declared-but-uninstalled plugin is a startup error, never a silent fetch.
func Binary(coord Coordinate, lock *Lock) (string, error) {
	store, err := DefaultStore()
	if err != nil {
		return "", err
	}
	return store.Binary(coord, lock)
}

func (s *Store) Binary(coord Coordinate, lock *Lock) (string, error) {
	report, err := s.Locate(coord, lock)
	if err != nil {
		return "", err
	}
	return report.Binary, nil
}

// Locate resolves a declaration to the binary that will run and re-verifies it.
// A digest mismatch refuses: it never warns and continues, and no flag makes it
// continue, because the thing being launched is code from someone else.
func (s *Store) Locate(coord Coordinate, lock *Lock) (Report, error) {
	name := coord.Name
	if err := checkName(name); err != nil {
		return Report{}, err
	}
	report := Report{Name: name, Origin: coord.Origin, Platform: s.platform.Key(), Warning: coord.Warning()}

	if coord.Origin == OriginLocal {
		info, err := os.Stat(coord.Path)
		if err != nil || info.IsDir() {
			return Report{}, internalerror.NewPreconditionError(Label(name)+" runs "+coord.Path+
				" in place, but there is no file there", err)
		}
		report.Binary, report.Version = coord.Path, string(OriginLocal)
		return report, nil
	}

	artifact, locked := lock.Artifact(name, s.platform)
	if !locked {
		return Report{}, internalerror.NewPreconditionError(Label(name)+" has no "+LockFileName+" entry for "+
			s.platform.Key()+" — run: lore plugin install "+name, nil)
	}
	entry, _ := lock.Entry(name)

	dir, err := s.Dir(name, entry.Version)
	if err != nil {
		return Report{}, err
	}
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
		return Report{}, digestMismatch(name, s.platform, expected, actual)
	}

	report.Version, report.Binary, report.BinaryDigest = entry.Version, binary, actual
	report.LockedURL, report.LockedDigest = artifact.URL, artifact.Digest
	return report, nil
}

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
		if entry.IsDir() || !isCacheEntryName(name) {
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

func isCacheEntryName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, `/\`) &&
		filepath.Base(name) == name
}

// write stores an unpacked binary and the digest re-checked at every launch.
func (s *Store) write(name, version, binaryName string, body []byte) (path, digest string, err error) {
	if !isCacheEntryName(binaryName) {
		return "", "", internalerror.NewPreconditionError(Label(name)+": the artifact names its binary "+
			binaryName+", which is not a usable file name: a binary is one file in the plugin cache, so it"+
			" must be a single name that does not start with a dot", nil)
	}

	dir, err := s.Dir(name, version)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", internalerror.NewInternalError("cannot create the plugin cache at "+dir, err)
	}

	path = filepath.Join(dir, binaryName)
	if err := fsx.WriteAtomic(path, body, 0o755); err != nil {
		return "", "", internalerror.NewInternalError("cannot write "+path, err)
	}

	digest = digestOf(body)
	if err := fsx.WriteAtomic(filepath.Join(dir, digestFileName), []byte(digest+"\n"), 0o644); err != nil {
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
	if err := checkName(name); err != nil {
		return 0, err
	}
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
	return internalerror.NewPreconditionError(Label(name)+" is not installed — run: lore plugin install "+name, nil)
}

func digestMismatch(name string, p Platform, expected, actual string) error {
	return internalerror.NewPreconditionError(Label(name)+": digest mismatch for "+p.Key()+
		" (expected "+expected+", got "+actual+")", nil)
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
