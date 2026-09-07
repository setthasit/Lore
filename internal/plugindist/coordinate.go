package plugindist

import (
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/urlx"
)

type Origin string

const (
	OriginLocal  Origin = "local"
	OriginGitHub Origin = "github"
	OriginURL    Origin = "url"
)

// LatestVersion is legal only as an argument to `lore plugin install`, which writes back what it resolved.
const LatestVersion = "latest"

const gitHubPrefix = "github.com/"

// archiveSuffixes are what the goreleaser convention publishes; any other artifact is taken to be the binary.
var archiveSuffixes = []string{".tar.gz", ".tgz"}

var versionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?$`)

func ExactVersion(version string) bool {
	return versionPattern.MatchString(version)
}

func checkName(name string) error {
	if registry.ValidPluginName(name) {
		return nil
	}
	return internalerror.NewBadRequestError(Label(name)+" is not a usable plugin name: a name is one"+
		" directory in the plugin cache, so it must be lower-case and hyphenated — name it the way `use:`"+
		" should read it, such as linear or openai-compatible", nil)
}

type Coordinate struct {
	Name   string
	Origin Origin
	From   string // the coordinate as declared, which is what lore.lock records
	PubKey string // absolute path to the key a signature must verify against; empty requires no signature

	Path string // OriginLocal: the file executed in place, with a leading ~ expanded

	Owner   string // OriginGitHub
	Repo    string // OriginGitHub
	Version string // OriginGitHub: the release tag; OriginURL: derived from the URL's last segment
	URL     string // OriginURL: the artifact URL, verbatim
}

// Resolve reads a declaration; a local from: and a relative pubkey: resolve against dir.
func Resolve(dir string, decl config.PluginDecl) (Coordinate, error) {
	return parseCoordinate(dir, decl, false)
}

// ResolveInstall is Resolve for `lore plugin install`, where @latest is legal.
func ResolveInstall(dir string, decl config.PluginDecl) (Coordinate, error) {
	return parseCoordinate(dir, decl, true)
}

func parseCoordinate(dir string, decl config.PluginDecl, allowLatest bool) (Coordinate, error) {
	name, from := strings.TrimSpace(decl.Name), strings.TrimSpace(decl.From)
	if name == "" {
		return Coordinate{}, internalerror.NewBadRequestError(
			"a plugin declaration needs a name: the token every `use:` refers to", nil)
	}
	if err := checkName(name); err != nil {
		return Coordinate{}, err
	}
	if from == "" {
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" declares no from: — a local path"+
			" (./bin/lore-"+name+"), github.com/owner/repo@vX.Y.Z, or an https:// artifact URL", nil)
	}

	var coord Coordinate
	var err error
	switch {
	case IsLocalPath(from):
		coord, err = parseLocal(dir, name, from)
	case strings.HasPrefix(from, gitHubPrefix):
		coord, err = parseGitHub(name, from, allowLatest)
	case strings.HasPrefix(from, "https://"):
		coord, err = parseURL(name, from)
	case strings.HasPrefix(from, "http://"):
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+safeTarget(from)+
			" is plaintext HTTP, which cannot carry code anyone should run: publish the artifact over https", nil)
	default:
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+urlx.RedactIfUserinfo(from)+
			" is not a coordinate — use a local path (./bin/lore-"+name+"), github.com/owner/repo@vX.Y.Z,"+
			" or an https:// artifact URL", nil)
	}
	if err != nil {
		return Coordinate{}, err
	}

	if decl.PubKey != "" {
		pubkey := strings.TrimSpace(decl.PubKey)
		if pubkey == "" {
			return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" declares a blank pubkey: — name"+
				" the public key a signature must verify against, or remove the line to install unsigned", nil)
		}
		if coord.PubKey, err = absolutePath(dir, pubkey, Label(name)+" pubkey:"); err != nil {
			return Coordinate{}, err
		}
	}
	return coord, nil
}

func IsLocalPath(from string) bool {
	for _, prefix := range []string{"./", "../", "/", "~/", `.\`, `..\`} {
		if strings.HasPrefix(from, prefix) {
			return true
		}
	}
	return false
}

// A declaration's name is a bare token; every coordinate shape carries a scheme or a path separator.
func isCoordinate(target string) bool {
	return strings.ContainsAny(target, `/\:`)
}

// The convention is lore-<name>; a repository that ignores it is declared by hand.
func nameFor(target string) (string, error) {
	_, repo, ok := gitHubRepo(target)
	if !ok {
		return "", internalerror.NewBadRequestError("install cannot derive a name for "+
			urlx.RedactIfUserinfo(target)+" — declare it under plugins: in lore.yaml with the name every"+
			" `use:` will refer to, then run: lore plugin install <name>", nil)
	}
	return strings.TrimPrefix(repo, "lore-"), nil
}

func gitHubRepo(from string) (owner, repo string, ok bool) {
	repoPart, isGitHub := strings.CutPrefix(from, gitHubPrefix)
	repoPart, _, _ = strings.Cut(repoPart, "@")
	owner, repo, split := strings.Cut(repoPart, "/")
	return owner, repo, isGitHub && split && owner != "" && repo != "" && !strings.Contains(repo, "/")
}

func parseLocal(dir, name, from string) (Coordinate, error) {
	absolute, err := absolutePath(dir, from, Label(name)+" from")
	if err != nil {
		return Coordinate{}, err
	}
	return Coordinate{Name: name, Origin: OriginLocal, From: from, Path: absolute}, nil
}

func parseGitHub(name, from string, allowLatest bool) (Coordinate, error) {
	owner, repo, ok := gitHubRepo(from)
	if !ok {
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+from+
			" names no repository — write github.com/owner/repo@vX.Y.Z", nil)
	}
	_, version, versioned := strings.Cut(from, "@")
	if !versioned || version == "" {
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+from+
			" pins no version — write "+from+"@v0.3.1, or run `lore plugin install "+name+
			"@latest` to pin the newest release", nil)
	}

	switch {
	case version == LatestVersion && !allowLatest:
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+from+
			" floats: @latest resolves differently on two machines, which would run different code against"+
			" one index — run `lore plugin install "+name+"@latest` to pin the version it resolves to now", nil)
	case version == LatestVersion:
	case !ExactVersion(version):
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+from+
			" pins @"+version+", which is not an exact version — pin a release tag like @v0.3.1", nil)
	}

	return Coordinate{
		Name: name, Origin: OriginGitHub, From: from,
		Owner: owner, Repo: repo, Version: version,
	}, nil
}

func parseURL(name, from string) (Coordinate, error) {
	parsed, err := url.Parse(from)
	if err != nil || parsed.Host == "" {
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+safeTarget(from)+
			" is not a valid URL", err)
	}

	version := path.Base(parsed.Path)
	for _, suffix := range archiveSuffixes {
		version = strings.TrimSuffix(version, suffix)
	}
	if !isCacheEntryName(version) {
		return Coordinate{}, internalerror.NewBadRequestError(Label(name)+" from "+safeTarget(from)+
			" ends in no version — the URL's last path segment names the version, as in"+
			" https://artifacts.example.com/lore/"+name+"/v2.0.1.tar.gz", nil)
	}

	return Coordinate{Name: name, Origin: OriginURL, From: from, Version: version, URL: from}, nil
}

func (c Coordinate) AtVersion(version string) (Coordinate, error) {
	if c.Origin != OriginGitHub {
		return Coordinate{}, internalerror.NewBadRequestError(Label(c.Name)+" is fetched from "+c.SafeFrom()+
			", so its version is part of that coordinate — edit from: in lore.yaml to move it", nil)
	}
	return parseCoordinate("", config.PluginDecl{
		Name:   c.Name,
		From:   gitHubPrefix + c.Owner + "/" + c.Repo + "@" + strings.TrimSpace(version),
		PubKey: c.PubKey,
	}, true)
}

// Only a URL coordinate's from: can carry a credential; the other shapes have nowhere to put one.
func (c Coordinate) SafeFrom() string {
	if c.Origin != OriginURL {
		return c.From
	}
	return safeTarget(c.From)
}

func (c Coordinate) Floating() bool {
	return c.Origin == OriginGitHub && c.Version == LatestVersion
}

func (c Coordinate) Warning() string {
	if c.Origin != OriginLocal {
		return ""
	}
	return Label(c.Name) + " runs " + c.Path + " in place: a local plugin is unpinned," +
		" has no lore.lock entry and no digest, and is for development only"
}

func (c Coordinate) assetName(p Platform) string {
	return c.Repo + "_" + strings.TrimPrefix(c.Version, "v") + "_" + p.OS + "_" + p.Arch + ".tar.gz"
}

// binaryName is the file the archive holds: goreleaser names it after the project, which is the repository.
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

// Platform is one os/arch pair, spelled the way GOOS and GOARCH spell it.
type Platform struct {
	OS   string
	Arch string
}

func hostPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

func (p Platform) Key() string {
	return p.OS + "/" + p.Arch
}

func Label(name string) string {
	return pluginsKey + "[" + name + "]"
}

func absolutePath(configDir, raw, field string) (string, error) {
	expanded, err := config.ExpandHome(field, raw)
	if err != nil {
		return "", err
	}

	// os/exec reads a separator-free name as a PATH lookup, so a declared path is made absolute, not cleaned.
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(configDir, expanded)
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", internalerror.NewBadRequestError(field+" "+raw+" cannot be resolved to a path", err)
	}
	return absolute, nil
}
