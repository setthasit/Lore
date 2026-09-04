package plugindist

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// LockFileName is committed beside lore.yaml. The digests live in their own
// file rather than inline in the configuration because they are per os/arch —
// a team on macOS with Linux CI needs both — and because they are generated:
// nobody edits them by hand, and a hand-written file nobody edits invites edits.
const LockFileName = "lore.lock"

// LockVersion is the only shape this build reads. A file from the future is
// refused rather than half-understood, since the thing it pins is code that
// will be executed.
const LockVersion = 1

const lockHeader = "# " + LockFileName + " — generated; written by `lore plugin install|update`\n"

// Lock records, per plugin, the version that runs and the digest of the
// artifact it came from, per platform. It is the only thing that decides which
// installed version runs: the cache may hold several, and it never picks.
type Lock struct {
	Version int                  `yaml:"version"`
	Plugins map[string]LockEntry `yaml:"plugins"`
}

type LockEntry struct {
	Version   string                  `yaml:"version"`
	From      string                  `yaml:"from"`
	Artifacts map[string]LockArtifact `yaml:"artifacts"`
}

type LockArtifact struct {
	URL    string `yaml:"url"`
	Digest string `yaml:"digest"`
}

// LoadLock reads the lockfile beside lore.yaml. A workspace with no external
// plugin has no lockfile, which is not an error: an empty lock is what "nothing
// is pinned yet" looks like, and every plugin declared against it then fails at
// startup with the command that pins it.
func LoadLock(dir string) (*Lock, error) {
	path := filepath.Join(dir, LockFileName)

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Lock{Version: LockVersion, Plugins: map[string]LockEntry{}}, nil
		}
		return nil, internalerror.NewInternalError("cannot read "+path, err)
	}
	defer func() { _ = file.Close() }()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	var lock Lock
	if err := decoder.Decode(&lock); err != nil && !errors.Is(err, io.EOF) {
		return nil, internalerror.NewBadRequestError("cannot parse "+path, err)
	}
	if lock.Plugins == nil {
		lock.Plugins = map[string]LockEntry{}
	}
	if lock.Version == 0 {
		lock.Version = LockVersion
	}
	if lock.Version != LockVersion {
		return nil, internalerror.NewPreconditionError(path+" is version "+strconv.Itoa(lock.Version)+
			", but this build reads version "+strconv.Itoa(LockVersion)+" — upgrade lore", nil)
	}
	return &lock, nil
}

// Save writes the lockfile. It is called once, after every requested install
// has succeeded, so an aborted install leaves the file on disk untouched.
func (l *Lock) Save(dir string) error {
	path := filepath.Join(dir, LockFileName)

	body, err := l.encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return internalerror.NewInternalError("cannot write "+path, err)
	}
	return nil
}

// encode renders the file. Map keys are emitted in sorted order by the encoder,
// so installing two plugins in either order produces the same bytes and a
// lockfile never shows up in a diff for having been rewritten.
func (l *Lock) encode() (string, error) {
	if l.Plugins == nil {
		l.Plugins = map[string]LockEntry{}
	}
	l.Version = LockVersion

	var body bytes.Buffer
	encoder := yaml.NewEncoder(&body)
	encoder.SetIndent(2)
	if err := encoder.Encode(l); err != nil {
		return "", internalerror.NewInternalError("cannot encode "+LockFileName, err)
	}
	if err := encoder.Close(); err != nil {
		return "", internalerror.NewInternalError("cannot encode "+LockFileName, err)
	}
	return lockHeader + body.String(), nil
}

// Entry reports what is locked for a plugin. A nil lock answers "nothing",
// which is what a workspace with no lockfile has to mean.
func (l *Lock) Entry(name string) (LockEntry, bool) {
	if l == nil {
		return LockEntry{}, false
	}
	entry, found := l.Plugins[name]
	return entry, found
}

// Artifact reports what is locked for a plugin on one platform. A plugin locked
// for another platform only is deliberately not a match: the digest that has to
// be checked is this platform's.
func (l *Lock) Artifact(name string, p Platform) (LockArtifact, bool) {
	entry, found := l.Entry(name)
	if !found {
		return LockArtifact{}, false
	}
	artifact, found := entry.Artifacts[p.Key()]
	return artifact, found
}

// Set records one platform's artifact, leaving the other platforms' entries
// intact: a macOS developer and Linux CI fill in the same file from two
// machines, and neither may drop the other's digest.
func (l *Lock) Set(name, version, from string, p Platform, artifact LockArtifact) {
	if l.Plugins == nil {
		l.Plugins = map[string]LockEntry{}
	}

	entry := l.Plugins[name]
	// A version change invalidates every platform's digest, because they are
	// digests of that version's artifacts and of nothing else.
	if entry.Version != version {
		entry.Artifacts = nil
	}
	if entry.Artifacts == nil {
		entry.Artifacts = map[string]LockArtifact{}
	}
	entry.Version, entry.From = version, from
	entry.Artifacts[p.Key()] = artifact
	l.Plugins[name] = entry
}

func (l *Lock) Remove(name string) bool {
	if l == nil {
		return false
	}
	if _, found := l.Plugins[name]; !found {
		return false
	}
	delete(l.Plugins, name)
	return true
}
