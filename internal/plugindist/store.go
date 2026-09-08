package plugindist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/fsx"
	"github.com/setthasit/Lore/sdk"
)

const RootEnv = "LORE_HOME"

const (
	recordFileName = ".install.json"

	manifestFileName = ".manifest.json"

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
			s.platform.Key()+reinstallRemedy(name), nil)
	}
	entry, _ := lock.Entry(name)

	dir, err := s.Dir(name, entry.Version)
	if err != nil {
		return Report{}, err
	}

	record, err := readInstallRecord(dir)
	if err != nil {
		if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
			return Report{}, notInstalled(name)
		}
		return Report{}, unreadableProvenance(name, err)
	}
	if !isCacheEntryName(record.Binary) {
		return Report{}, unreadableProvenance(name, errors.New("binary "+strconv.Quote(record.Binary)+
			" is not one file name"))
	}

	binary := filepath.Join(dir, record.Binary)
	actual, err := digestFile(binary)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Report{}, unreadableProvenance(name, err)
		}
		return Report{}, err
	}
	if record.BinaryDigest != actual {
		return Report{}, cachedBinaryMismatch(name, s.platform, record.BinaryDigest, actual)
	}

	if !sameFrom(record.From, entry.From) || record.ArtifactDigest != artifact.Digest {
		return Report{}, provenanceMismatch(name, s.platform, record, safeFrom(entry.From), artifact.Digest)
	}

	report.Version, report.Binary, report.BinaryDigest = entry.Version, binary, actual
	report.LockedURL, report.LockedDigest = safeTarget(artifact.URL), artifact.Digest
	return report, nil
}

func isCacheEntryName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, `/\`) &&
		filepath.Base(name) == name
}

func redactFrom(from string) (safe string, readable bool) {
	if !strings.Contains(from, "://") {
		return from, true
	}
	_, err := url.Parse(from)
	return safeTarget(from), err == nil
}

func safeFrom(from string) string {
	safe, _ := redactFrom(from)
	return safe
}

func lockedOrigin(from string) string {
	if from == "" {
		return "an unrecorded origin"
	}
	return from
}

func sameFrom(a, b string) bool {
	left, leftReadable := redactFrom(a)
	right, rightReadable := redactFrom(b)
	return leftReadable && rightReadable && left == right
}

func (s *Store) write(
	coord Coordinate,
	binaryName string,
	body []byte,
	artifactDigest string,
) (path string, record installRecord, err error) {
	if !isCacheEntryName(binaryName) {
		return "", installRecord{}, internalerror.NewPreconditionError(Label(coord.Name)+": the artifact names"+
			" its binary "+binaryName+", which is not a usable file name: a binary is one file in the plugin"+
			" cache, so it must be a single name that does not start with a dot", nil)
	}

	dir, err := s.Dir(coord.Name, coord.Version)
	if err != nil {
		return "", installRecord{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", installRecord{}, internalerror.NewInternalError("cannot create the plugin cache at "+dir, err)
	}

	path = filepath.Join(dir, binaryName)
	if err := fsx.WriteAtomic(path, body, 0o755); err != nil {
		return "", installRecord{}, internalerror.NewInternalError("cannot write "+path, err)
	}
	return path, installRecord{
		Binary:         binaryName,
		BinaryDigest:   digestOf(body),
		ArtifactDigest: artifactDigest,
		From:           coord.SafeFrom(),
	}, nil
}

func (s *Store) recordInstall(coord Coordinate, record installRecord, manifest lore.Manifest) error {
	dir, err := s.Dir(coord.Name, coord.Version)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, manifestFileName)
	if err := writeJSON(path, manifest); err != nil {
		return internalerror.NewInternalError("cannot write "+path, err)
	}

	record.Manifest = manifestFileName
	if err := writeInstallRecord(dir, record); err != nil {
		return internalerror.NewInternalError("cannot record the provenance of "+
			filepath.Join(dir, record.Binary), err)
	}
	return nil
}

type installRecord struct {
	Binary         string `json:"binary"`
	BinaryDigest   string `json:"binary_digest"`
	ArtifactDigest string `json:"artifact_digest"`
	From           string `json:"from"`
	Manifest       string `json:"manifest,omitempty"`
}

func writeInstallRecord(dir string, record installRecord) error {
	return writeJSON(filepath.Join(dir, recordFileName), record)
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(path, append(body, '\n'), 0o644)
}

func readInstallRecord(dir string) (installRecord, error) {
	body, err := os.ReadFile(filepath.Join(dir, recordFileName))
	if err != nil {
		return installRecord{}, err
	}

	var record installRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return installRecord{}, err
	}
	return record, nil
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

func reinstallRemedy(name string) string {
	return " — run: lore plugin install " + name
}

func notInstalled(name string) error {
	return internalerror.NewPreconditionError(Label(name)+" is not installed"+reinstallRemedy(name), nil)
}

func digestMismatch(name string, p Platform, expected, actual string) error {
	return internalerror.NewPreconditionError(Label(name)+": digest mismatch for "+p.Key()+
		" (expected "+expected+", got "+actual+")", nil)
}

func cachedBinaryMismatch(name string, p Platform, recorded, actual string) error {
	return internalerror.NewPreconditionError(Label(name)+": digest mismatch for "+p.Key()+
		" — the cached binary hashes to "+actual+", not the recorded "+recorded+reinstallRemedy(name), nil)
}

func unreadableProvenance(name string, cause error) error {
	return internalerror.NewPreconditionError(Label(name)+": cannot read the recorded provenance of the"+
		" cached install"+reinstallRemedy(name), cause)
}

func provenanceMismatch(name string, p Platform, record installRecord, pinnedFrom, pinnedDigest string) error {
	return internalerror.NewPreconditionError(Label(name)+": digest mismatch for "+p.Key()+
		" — the cache holds the install of "+safeFrom(record.From)+" at artifact "+record.ArtifactDigest+", but "+
		LockFileName+" pins "+lockedOrigin(pinnedFrom)+" at "+pinnedDigest+reinstallRemedy(name), nil)
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
