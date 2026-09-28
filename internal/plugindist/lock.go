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
	"github.com/setthasit/Lore/internal/fsx"
)

const LockFileName = "lore.lock"

const lockVersion = 1

const lockHeader = "# " + LockFileName + " — generated; written by `lore plugin install|update`\n"

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

// LoadLock treats a missing lockfile as an empty lock rather than an error: an unpinned workspace is legal.
func LoadLock(dir string) (*Lock, error) {
	path := filepath.Join(dir, LockFileName)

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Lock{Version: lockVersion, Plugins: map[string]LockEntry{}}, nil
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
		lock.Version = lockVersion
	}
	if lock.Version != lockVersion {
		return nil, internalerror.NewPreconditionError(path+" is version "+strconv.Itoa(lock.Version)+
			", but this build reads version "+strconv.Itoa(lockVersion)+" — upgrade lore", nil)
	}
	return &lock, nil
}

func (l *Lock) Save(dir string) error {
	path := filepath.Join(dir, LockFileName)

	body, err := l.encode()
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(path, []byte(body), fsx.ModeOf(path, 0o644)); err != nil {
		return internalerror.NewInternalError("cannot write "+path, err)
	}
	return nil
}

// The encoder emits map keys sorted, so installing two plugins in either order produces the same bytes.
func (l *Lock) encode() (string, error) {
	if l.Plugins == nil {
		l.Plugins = map[string]LockEntry{}
	}
	l.Version = lockVersion

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

// A nil lock answers "nothing", which is what a workspace with no lockfile means.
func (l *Lock) Entry(name string) (LockEntry, bool) {
	if l == nil {
		return LockEntry{}, false
	}
	entry, found := l.Plugins[name]
	return entry, found
}

func (l *Lock) Artifact(name string, p Platform) (LockArtifact, bool) {
	entry, found := l.Entry(name)
	if !found {
		return LockArtifact{}, false
	}
	artifact, found := entry.Artifacts[p.Key()]
	return artifact, found
}

func (l *Lock) Set(name, version, from string, p Platform, artifact LockArtifact) {
	if l.Plugins == nil {
		l.Plugins = map[string]LockEntry{}
	}

	entry := l.Plugins[name]
	if entry.Version != version || !sameFrom(entry.From, from) {
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
