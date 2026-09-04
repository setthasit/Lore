package plugindist

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// Origin is the shape of a `from:` coordinate, and the only thing that decides
// how a binary is obtained. Nothing downstream of resolution depends on it:
// once a binary is on disk it is launched the same way whatever brought it.
type Origin string

const (
	OriginLocal  Origin = "local"
	OriginGitHub Origin = "github"
	OriginURL    Origin = "url"
)

func (o Origin) String() string {
	return string(o)
}

// LatestVersion is the one floating version the supply chain understands, and
// `lore plugin install` is the only place it may appear: install resolves it
// and writes the concrete version back, because a floating version means two
// machines silently run different code against one index.
const LatestVersion = "latest"

const gitHubPrefix = "github.com/"

// archiveSuffixes are the archive shapes the goreleaser convention publishes.
// Anything else served at a URL is taken to be the binary itself.
var archiveSuffixes = []string{".tar.gz", ".tgz"}

// versionPattern is what "pinned to an exact version" means here. A branch or a
// moving tag is refused rather than resolved, because the tag a lockfile
// records must not be able to start pointing somewhere else.
var versionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?$`)

// Coordinate is a resolved `from:` declaration. It carries the plugin's short
// name because every message about a plugin names it the way lore.yaml does,
// as plugins[<name>].
type Coordinate struct {
	Name   string
	Origin Origin
	From   string // the coordinate as declared, which is what lore.lock records

	Path string // OriginLocal: the file executed in place, with a leading ~ expanded

	Owner   string // OriginGitHub
	Repo    string // OriginGitHub
	Version string // OriginGitHub: the release tag; OriginURL: derived from the URL's last segment
	URL     string // OriginURL: the artifact URL, verbatim
}

// Resolve reads the coordinate a declaration names. A remote coordinate must
// pin an exact version here, so a workspace's configuration alone determines
// which code runs.
//
// dir is the directory the declaring lore.yaml sits in: a local path means
// "beside the configuration that declared it", so a workspace behaves the same
// whichever directory lore was started from.
func Resolve(dir string, decl config.PluginDecl) (Coordinate, error) {
	return parseCoordinate(dir, decl.Name, decl.From, false)
}

// ResolveInstall reads a coordinate `lore plugin install` was handed, where
// @latest is legal: install resolves it and writes the concrete version back
// into lore.yaml before anything is locked or executed.
func ResolveInstall(dir, name, from string) (Coordinate, error) {
	return parseCoordinate(dir, name, from, true)
}

func parseCoordinate(dir, name, from string, allowLatest bool) (Coordinate, error) {
	name, from = strings.TrimSpace(name), strings.TrimSpace(from)
	if name == "" {
		return Coordinate{}, internalerror.NewBadRequestError(
			"a plugin declaration needs a name: the token every `use:` refers to", nil)
	}
	if from == "" {
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" declares no from: — a local path"+
			" (./bin/lore-"+name+"), github.com/owner/repo@vX.Y.Z, or an https:// artifact URL", nil)
	}

	switch {
	case isLocalPath(from):
		return parseLocal(dir, name, from)
	case strings.HasPrefix(from, gitHubPrefix):
		return parseGitHub(name, from, allowLatest)
	case strings.HasPrefix(from, "https://"):
		return parseURL(name, from)
	case strings.HasPrefix(from, "http://"):
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" is plaintext HTTP, which cannot carry code anyone should run: publish the artifact over https", nil)
	default:
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" is not a coordinate — use a local path (./bin/lore-"+name+"), github.com/owner/repo@vX.Y.Z,"+
			" or an https:// artifact URL", nil)
	}
}

// A local coordinate is recognised by its shape alone, exactly as documented,
// so a bare token can never be mistaken for a path and executed by accident.
func isLocalPath(from string) bool {
	for _, prefix := range []string{"./", "../", "/", "~/", `.\`, `..\`} {
		if strings.HasPrefix(from, prefix) {
			return true
		}
	}
	return false
}

func parseLocal(dir, name, from string) (Coordinate, error) {
	expanded, err := expandHome(name, from)
	if err != nil {
		return Coordinate{}, err
	}

	// The path is made absolute rather than merely cleaned. os/exec treats a
	// name with no separator as a PATH lookup, so a cleaned "./plugin" would
	// send the host hunting through PATH for something sitting right here.
	path := expanded
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" cannot be resolved to a path", err)
	}
	return Coordinate{Name: name, Origin: OriginLocal, From: from, Path: absolute}, nil
}

func parseGitHub(name, from string, allowLatest bool) (Coordinate, error) {
	repoPart, version, versioned := strings.Cut(strings.TrimPrefix(from, gitHubPrefix), "@")

	segments := strings.Split(repoPart, "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" names no repository — write github.com/owner/repo@vX.Y.Z", nil)
	}
	if !versioned || version == "" {
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" pins no version — write "+from+"@v0.3.1, or run `lore plugin install "+name+
			"@latest` to pin the newest release", nil)
	}

	switch {
	case version == LatestVersion && !allowLatest:
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" floats: @latest resolves differently on two machines, which would run different code against"+
			" one index — run `lore plugin install "+name+"@latest` to pin the version it resolves to now", nil)
	case version == LatestVersion:
	case !versionPattern.MatchString(version):
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" pins @"+version+", which is not an exact version — pin a release tag like @v0.3.1", nil)
	}

	return Coordinate{
		Name: name, Origin: OriginGitHub, From: from,
		Owner: segments[0], Repo: segments[1], Version: version,
	}, nil
}

// A URL coordinate is its own pin: the bytes it serves are pinned by the
// lockfile digest, and the version is read from the file name because the
// on-disk layout keeps versions in separate directories.
func parseURL(name, from string) (Coordinate, error) {
	parsed, err := url.Parse(from)
	if err != nil || parsed.Host == "" {
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+" is not a valid URL", err)
	}

	version := path.Base(parsed.Path)
	for _, suffix := range archiveSuffixes {
		version = strings.TrimSuffix(version, suffix)
	}
	if version == "" || version == "." || version == ".." || version == "/" || strings.ContainsAny(version, `/\`) {
		return Coordinate{}, internalerror.NewBadRequestError(label(name)+" from "+from+
			" ends in no version — the URL's last path segment names the version, as in"+
			" https://artifacts.example.com/lore/"+name+"/v2.0.1.tar.gz", nil)
	}

	return Coordinate{Name: name, Origin: OriginURL, From: from, Version: version, URL: from}, nil
}

// AtVersion re-points a coordinate at another release, which is what
// `lore plugin update <name>@<version>` asks for. A URL coordinate's version is
// part of the URL, so moving that one is an edit to lore.yaml, not an argument.
func (c Coordinate) AtVersion(version string) (Coordinate, error) {
	if c.Origin != OriginGitHub {
		return Coordinate{}, internalerror.NewBadRequestError(label(c.Name)+" is fetched from "+c.From+
			", so its version is part of that coordinate — edit from: in lore.yaml to move it", nil)
	}
	return parseCoordinate("", c.Name, gitHubPrefix+c.Owner+"/"+c.Repo+"@"+strings.TrimSpace(version), true)
}

// Remote reports whether the coordinate must be fetched, verified and locked.
func (c Coordinate) Remote() bool {
	return c.Origin != OriginLocal
}

// Floating reports a coordinate that still has to be pinned, so nothing
// unpinned can reach the lockfile or the cache.
func (c Coordinate) Floating() bool {
	return c.Origin == OriginGitHub && c.Version == LatestVersion
}

// Warning is what the host logs at startup, empty when there is nothing to say.
// A local plugin is the development escape hatch, and its whole cost is stated
// where the person who chose it will see it.
func (c Coordinate) Warning() string {
	if c.Origin != OriginLocal {
		return ""
	}
	return label(c.Name) + " runs " + c.Path + " in place: a local plugin is unpinned," +
		" has no lore.lock entry and no digest, and is for development only"
}

// AssetName is the archive the goreleaser default naming publishes for one
// platform. It is constructed rather than guessed at from a release's
// attachments, so a release that does not follow the convention fails to
// resolve with the exact name that was looked for instead of running something
// that merely looked close.
func (c Coordinate) AssetName(p Platform) string {
	return c.Repo + "_" + strings.TrimPrefix(c.Version, "v") + "_" + p.OS + "_" + p.Arch + ".tar.gz"
}

// binaryName is the file the archive is expected to hold: goreleaser names it
// after the project, which for a plugin repository is the repository itself.
func (c Coordinate) binaryName(p Platform) string {
	base := c.Repo
	if base == "" {
		base = c.Name
	}
	if p.OS == "windows" {
		return base + ".exe"
	}
	return base
}

// Platform is one os/arch pair, spelled the way GOOS and GOARCH spell it: it is
// both a lockfile key and part of an asset name, so the two cannot drift.
type Platform struct {
	OS   string
	Arch string
}

func HostPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

func (p Platform) Key() string {
	return p.OS + "/" + p.Arch
}

func (p Platform) String() string {
	return p.Key()
}

// label spells a plugin the way lore.yaml does, so an error points at the line
// the reader has to edit.
func label(name string) string {
	return "plugins[" + name + "]"
}

// Only a leading "~" is expanded, matching how the rest of the configuration
// treats a path.
func expandHome(name, raw string) (string, error) {
	if !strings.HasPrefix(raw, "~/") {
		return raw, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", internalerror.NewBadRequestError(label(name)+" from "+raw+
			" starts with ~, but this user has no home directory; declare an absolute path", err)
	}
	return filepath.Join(home, strings.TrimPrefix(raw, "~/")), nil
}
