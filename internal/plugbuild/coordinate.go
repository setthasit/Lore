package plugbuild

import (
	"go/token"
	"path"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
)

// Trimming this repository prefix derives the package name: github.com/jdoe/lore-linear holds package linear.
const repoPrefix = "lore-"

// reservedPackages are the identifiers the generated composition root already spends.
var reservedPackages = map[string]bool{
	"os":      true,
	"app":     true,
	"plugins": true,
	"main":    true,
	"append":  true,
}

// Coordinate is one module to compile in. Version is always exact: the scratch
// module's go.mod is discarded, so no lockfile records what @latest resolved to.
type Coordinate struct {
	Module  string
	Version string
	Package string
}

func (c Coordinate) String() string { return c.Module + "@" + c.Version }

// ParseCoordinate reads `github.com/owner/repo@vX.Y.Z[=package]`.
func ParseCoordinate(raw string) (Coordinate, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return Coordinate{}, badCoordinate(raw, "it is empty")
	}
	if plugindist.IsLocalPath(spec) {
		return Coordinate{}, badCoordinate(raw, "lore build takes module coordinates, not paths — "+
			"a local plugin runs out of process, declared as `from: "+spec+"` under plugins: in lore.yaml")
	}

	spec, pkg, explicit := cutPackage(spec)

	module, version, _ := strings.Cut(spec, "@")
	if err := checkModule(raw, module); err != nil {
		return Coordinate{}, err
	}
	if err := checkVersion(raw, module, version); err != nil {
		return Coordinate{}, err
	}

	if explicit {
		if !isPackageName(pkg) {
			return Coordinate{}, badCoordinate(raw, "="+pkg+" is not a Go package name")
		}
	} else {
		derived, err := derivePackage(raw, module, version)
		if err != nil {
			return Coordinate{}, err
		}
		pkg = derived
	}

	return Coordinate{Module: module, Version: version, Package: pkg}, nil
}

func cutPackage(spec string) (rest, pkg string, explicit bool) {
	i := strings.LastIndex(spec, "=")
	if i < 0 {
		return spec, "", false
	}
	return spec[:i], spec[i+1:], true
}

func checkModule(raw, module string) error {
	switch {
	case module == "":
		return badCoordinate(raw, "it names no module")
	case strings.ContainsAny(module, " \t\"'"):
		return badCoordinate(raw, "the module path contains whitespace or quotes")
	case module[0] == '.' || module[0] == '~' || module[0] == '-':
		return badCoordinate(raw, "a module path begins with a host, not "+module[:1]+
			" — write github.com/owner/repo")
	case !strings.Contains(module, "/"):
		return badCoordinate(raw, "a module path needs a host, as in github.com/owner/repo")
	}
	return nil
}

func checkVersion(raw, module, version string) error {
	switch {
	case version == "", version == "v":
		return badCoordinate(raw, "it names no version — write "+module+"@v0.3.1")
	case !plugindist.ExactVersion(version):
		return badCoordinate(raw, "the version must be an exact module version tag — write "+module+"@v0.3.1")
	}
	return nil
}

func derivePackage(raw, module, version string) (string, error) {
	base := path.Base(module)
	if isMajorSuffix(base) {
		base = path.Base(path.Dir(module))
	}

	name := strings.TrimPrefix(base, repoPrefix)
	switch {
	case !isPackageName(name):
		return "", badCoordinate(raw, "the Go package name cannot be derived from "+module+
			" — append the package, as in --with "+module+"@"+version+"=acmecrm")
	case reservedPackages[name]:
		return "", badCoordinate(raw, "the derived package name "+name+
			" is already taken in the generated composition root — append the package, as in --with "+
			module+"@"+version+"=acmecrm")
	}
	return name, nil
}

// A /vN element names no package: github.com/acme/lore-crm/v2 still holds package crm.
func isMajorSuffix(element string) bool {
	if len(element) < 2 || element[0] != 'v' {
		return false
	}
	for _, r := range element[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isPackageName(name string) bool {
	return token.IsIdentifier(name) && !token.IsKeyword(name)
}

func badCoordinate(raw, reason string) error {
	return internalerror.NewBadRequestError("--with "+raw+": "+reason, nil)
}
