package plugbuild

import (
	"go/token"
	"path"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// repoPrefix is the naming convention the distribution document fixes for a
// plugin repository, so trimming it is the one derivation that is a convention
// rather than a guess: github.com/jdoe/lore-linear holds package linear.
const repoPrefix = "lore-"

// reservedPackages are the identifiers the generated composition root already
// spends. A plugin package landing on one of them would shadow os, the engine,
// the official plugin set, package main or the builtin the root calls, so it
// has to be named explicitly instead of silently breaking generated code.
var reservedPackages = map[string]bool{
	"os":      true,
	"app":     true,
	"plugins": true,
	"main":    true,
	"append":  true,
}

// Coordinate is one module to compile in. Version is always exact: a binary
// built from a floating version cannot be rebuilt into the same binary, and a
// compiled plugin has no lockfile entry to record what @latest resolved to.
type Coordinate struct {
	Module  string // github.com/jdoe/lore-linear
	Version string // v0.3.1
	Package string // the package name the generated root calls Plugin() on
}

// String renders the coordinate the way it is written on the command line,
// without the package suffix: that suffix is a detail of code generation.
func (c Coordinate) String() string { return c.Module + "@" + c.Version }

// Query is what the go command is asked to fetch.
func (c Coordinate) Query() string { return c.Module + "@" + c.Version }

// ParseCoordinate reads `github.com/owner/repo@vX.Y.Z` with an optional
// `=<package>` suffix. The package name is derived from the module path only
// where the convention makes that unambiguous; anything else is an error asking
// for the suffix, because a wrong guess surfaces as a compile failure inside
// generated code the user never wrote.
func ParseCoordinate(raw string) (Coordinate, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return Coordinate{}, badCoordinate(raw, "it is empty")
	}
	if strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "~") {
		return Coordinate{}, badCoordinate(raw, "lore build takes module coordinates, not paths — "+
			"a local plugin runs out of process, declared as `from: "+spec+"` under plugins: in lore.yaml")
	}

	spec, pkg, explicit := cutPackage(spec)

	module, version, found := strings.Cut(spec, "@")
	if !found {
		return Coordinate{}, badCoordinate(raw, "it names no version — write "+module+"@v0.3.1")
	}
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

// The package suffix is cut before the version is, and on the last =, because a
// module path may not contain an = at all: the split cannot be ambiguous.
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
	case !strings.Contains(module, "/"):
		return badCoordinate(raw, "a module path needs a host, as in github.com/owner/repo")
	}
	return nil
}

// @latest is refused here even though `lore plugin install` accepts it: install
// writes the version it resolved back into lore.yaml, while a custom binary
// records nothing, so a floating query would leave no evidence anywhere of what
// was compiled in.
func checkVersion(raw, module, version string) error {
	switch {
	case version == "", version == "v":
		return badCoordinate(raw, "it names no version — write "+module+"@v0.3.1")
	case !strings.HasPrefix(version, "v"):
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

// isMajorSuffix reports whether the element is a /vN major-version suffix,
// which names no package: github.com/acme/lore-crm/v2 still holds package crm.
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
