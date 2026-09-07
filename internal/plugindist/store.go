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

const RootEnv = "LORE_HOME"

const (
	// digestFileName holds the digest re-checked at every launch, so a binary rewritten after install is caught too.
	digestFileName = ".digest"

	pluginsDirName = "plugins"
)

type Store struct {
	root     string
	platform Platform
}

func NewStore(root string) *Store {
	return &Store{root: root, platform: hostPlatform()}
}

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

// Binary never downloads: a declared-but-uninstalled plugin is an error naming the command that installs it.
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

// Locate re-verifies the binary against its recorded digest; a mismatch refuses, and no flag makes it continue.
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
		// Without the digest file the binary can never be launched, so the half-installed version goes.
		_ = os.RemoveAll(dir)
		return "", "", internalerror.NewInternalError("cannot record the digest of "+path, err)
	}
	return path, digest, nil
}

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

const digestPrefix = "sha256:"

func encodeDigest(sum []byte) string {
	return digestPrefix + hex.EncodeToString(sum)
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return encodeDigest(sum[:])
}

// digestFile streams the file: this runs at every startup, for a binary that may be tens of megabytes.
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
	return encodeDigest(hash.Sum(nil)), nil
}
