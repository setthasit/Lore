package plugindist

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/urlx"
)

const pluginsKey = "plugins"

type Workspace struct {
	path      string
	dir       string
	content   string // kept beside the parsed config: every edit to a hand-written lore.yaml is a splice
	config    *config.Config
	lock      *Lock
	store     *Store
	installer *Installer
}

func Open(configPath string) (*Workspace, error) {
	content, parsed, err := config.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(configPath)
	lock, err := LoadLock(dir)
	if err != nil {
		return nil, err
	}
	store, err := DefaultStore()
	if err != nil {
		return nil, err
	}
	return &Workspace{
		path: configPath, dir: dir, content: content,
		config: parsed, lock: lock, store: store, installer: NewInstaller(store),
	}, nil
}

func (w *Workspace) Plugins() []config.PluginDecl {
	return w.config.Plugins
}

// A Fault is the store's refusal; an empty Binary with no Fault means nothing is installed.
type Installation struct {
	Binary string
	Fault  error
}

func (w *Workspace) Installed(decl config.PluginDecl) (Installation, error) {
	coord, err := Resolve(w.dir, decl)
	if err != nil {
		return Installation{}, err
	}

	binary, err := w.store.Binary(coord, w.lock)
	switch {
	case err == nil:
		return Installation{Binary: binary}, nil
	case w.absent(coord):
		return Installation{}, nil
	default:
		return Installation{Fault: err}, nil
	}
}

func (w *Workspace) absent(coord Coordinate) bool {
	path := coord.Path
	if coord.Origin != OriginLocal {
		entry, pinned := w.lock.Entry(coord.Name)
		if !pinned {
			return true
		}
		dir, err := w.store.Dir(coord.Name, entry.Version)
		if err != nil {
			return false
		}
		path = dir
	}

	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// notice is called once the work is known, before the first byte is fetched; no results means nothing to install.
func (w *Workspace) Install(ctx context.Context, args []string, notice func()) ([]Result, error) {
	requests, declare, err := w.requests(args)
	if err != nil || len(requests) == 0 {
		return nil, err
	}
	notice()

	pinned := make([]Coordinate, 0, len(requests))
	for _, request := range requests {
		coord, err := w.installer.Pin(ctx, request, w.lock)
		if err != nil {
			return nil, err
		}
		pinned = append(pinned, coord)
	}
	edits := pinEdits(requests, pinned, declare)

	results := make([]Result, 0, len(requests))
	for i, request := range requests {
		request.Coordinate = pinned[i]
		result, err := w.installer.Install(ctx, request, w.lock)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	if err := w.commitPins(edits, anyPinned(results)); err != nil {
		return nil, err
	}
	return results, nil
}

func (w *Workspace) Update(ctx context.Context, argument string, notice func()) (Result, error) {
	name, version := splitArgument(argument)
	decl, err := w.mustDeclare(name)
	if err != nil {
		return Result{}, err
	}
	coord, err := ResolveInstall(w.dir, decl)
	if err != nil {
		return Result{}, err
	}
	if coord.Origin == OriginLocal {
		return Result{}, neverPinned(name)
	}

	switch {
	case version != "" && version != LatestVersion:
		if coord, err = coord.AtVersion(version); err != nil {
			return Result{}, err
		}
	case coord.Origin == OriginGitHub:
		if coord, err = coord.AtVersion(LatestVersion); err != nil {
			return Result{}, err
		}
	}
	notice()

	req := Request{Coordinate: coord, Rewrite: true}
	pinned, err := w.installer.Pin(ctx, req, w.lock)
	if err != nil {
		return Result{}, err
	}
	req.Coordinate = pinned
	result, err := w.installer.Install(ctx, req, w.lock)
	if err != nil {
		return Result{}, err
	}

	edits := []configEdit(nil)
	if pinned.From != decl.From {
		edits = append(edits, configEdit{name: name, from: pinned.From, kind: editPin})
	}
	if err := w.commitPins(edits, true); err != nil {
		return Result{}, err
	}
	return result, nil
}

type Removal struct {
	Unlocked bool
	Versions int
}

// The cache goes last, so a refused write to lore.yaml leaves it intact.
func (w *Workspace) Remove(name string) (Removal, error) {
	if _, err := w.mustDeclare(name); err != nil {
		return Removal{}, err
	}
	if _, err := config.FindBlock(w.content, pluginsKey); err != nil {
		return Removal{}, err
	}
	if used := w.config.InstancesUsing(name); len(used) > 0 {
		return Removal{}, internalerror.NewPreconditionError(Label(name)+" is still used by "+
			strings.Join(used, ", ")+
			" — remove those first, or the next `lore sync` has nothing to build them from", nil)
	}

	unlocked := w.lock.Remove(name)
	refusal := "removing " + Label(name) + " would leave " + w.path + " unreadable, so it is unchanged"
	if err := w.commit([]configEdit{{name: name, kind: editRemove}}, refusal, unlocked); err != nil {
		return Removal{}, err
	}

	versions, err := w.store.Remove(name)
	if err != nil {
		return Removal{}, err
	}
	return Removal{Unlocked: unlocked, Versions: versions}, nil
}

// Reaching a Report is itself the digest's verification: Locate re-hashes.
func (w *Workspace) Verify(name string) (Report, error) {
	decl, err := w.mustDeclare(name)
	if err != nil {
		return Report{}, err
	}
	coord, err := Resolve(w.dir, decl)
	if err != nil {
		return Report{}, err
	}
	return w.store.Locate(coord, w.lock)
}

// The second result is the name the argument introduces, empty when lore.yaml already declares it.
func (w *Workspace) requests(args []string) ([]Request, string, error) {
	if len(args) == 0 {
		requests := make([]Request, 0, len(w.config.Plugins))
		for _, decl := range w.config.Plugins {
			coord, err := Resolve(w.dir, decl)
			if err != nil {
				return nil, "", err
			}
			requests = append(requests, Request{Coordinate: coord})
		}
		return requests, "", nil
	}

	target, version := splitArgument(args[0])
	if !isCoordinate(target) {
		decl, err := w.mustDeclare(target)
		if err != nil {
			return nil, "", err
		}
		if version != "" && version != LatestVersion {
			return nil, "", internalerror.NewBadRequestError("install pins what lore.yaml declares; to move "+
				target+" to "+version+" run: lore plugin update "+target+"@"+version, nil)
		}

		coord, err := ResolveInstall(w.dir, decl)
		if err != nil {
			return nil, "", err
		}
		if version == LatestVersion {
			if coord.Origin == OriginLocal {
				return nil, "", neverPinned(target)
			}
			if coord, err = coord.AtVersion(LatestVersion); err != nil {
				return nil, "", err
			}
		}
		return []Request{{Coordinate: coord}}, "", nil
	}

	name, err := nameFor(target)
	if err != nil {
		return nil, "", err
	}
	from := target
	if version != "" {
		from = target + "@" + version
	}

	decl, declared := w.declaration(name)
	coord, err := ResolveInstall(w.dir, config.PluginDecl{Name: name, From: from, PubKey: decl.PubKey})
	if err != nil {
		return nil, "", err
	}
	if declared {
		if !matchesDeclaration(decl.From, coord, version) {
			return nil, "", internalerror.NewBadRequestError(w.path+" declares "+name+" from "+
				urlx.RedactIfUserinfo(decl.From)+", not "+coord.SafeFrom()+
				" — edit the declaration, or run: lore plugin update "+name, nil)
		}
		return []Request{{Coordinate: coord}}, "", nil
	}
	return []Request{{Coordinate: coord}}, name, nil
}

func matchesDeclaration(declared string, coord Coordinate, version string) bool {
	if version != LatestVersion {
		return declared == coord.From
	}
	return sameRepo(declared, coord)
}

func neverPinned(name string) error {
	return internalerror.NewBadRequestError(Label(name)+" is a local path, which is never pinned:"+
		" rebuild it in place instead", nil)
}

func pinEdits(requests []Request, pinned []Coordinate, declare string) []configEdit {
	edits := []configEdit(nil)
	for i, coord := range pinned {
		switch {
		case coord.Name == declare:
			edits = append(edits, configEdit{name: coord.Name, from: coord.From, kind: editDeclare})
		case coord.From != requests[i].Coordinate.From:
			edits = append(edits, configEdit{name: coord.Name, from: coord.From, kind: editPin})
		}
	}
	return edits
}

func (w *Workspace) declaration(name string) (config.PluginDecl, bool) {
	for _, decl := range w.config.Plugins {
		if decl.Name == name {
			return decl, true
		}
	}
	return config.PluginDecl{}, false
}

func (w *Workspace) mustDeclare(name string) (config.PluginDecl, error) {
	decl, found := w.declaration(name)
	if found {
		return decl, nil
	}

	declared := "none"
	if names := w.declaredNames(); len(names) > 0 {
		declared = strings.Join(names, ", ")
	}
	return config.PluginDecl{}, internalerror.NewNotFoundError(w.path+" declares no plugin called "+name+
		" — it declares: "+declared, nil)
}

func (w *Workspace) declaredNames() []string {
	names := make([]string, 0, len(w.config.Plugins))
	for _, decl := range w.config.Plugins {
		names = append(names, decl.Name)
	}
	return names
}

func (w *Workspace) commit(edits []configEdit, refusal string, lockChanged bool) error {
	if len(edits) > 0 {
		updated := w.content
		for _, edit := range edits {
			applied, err := edit.apply(updated)
			if err != nil {
				return err
			}
			updated = applied
		}
		if err := config.WriteFile(w.path, updated, refusal); err != nil {
			return err
		}
		w.content = updated
	}

	if lockChanged {
		return w.lock.Save(w.dir)
	}
	return nil
}

func (w *Workspace) commitPins(edits []configEdit, lockChanged bool) error {
	return w.commit(edits, "the pinned coordinates do not fit "+w.path+", which is unchanged", lockChanged)
}

func anyPinned(results []Result) bool {
	for _, result := range results {
		if result.Pinned {
			return true
		}
	}
	return false
}

func splitArgument(argument string) (target, version string) {
	argument = strings.TrimSpace(argument)
	versionSeparator := strings.LastIndex(argument, "@")
	if versionSeparator <= 0 || versionSeparator < strings.LastIndex(argument, "/") {
		return argument, ""
	}
	return argument[:versionSeparator], argument[versionSeparator+1:]
}

type configEdit struct {
	name string
	from string
	kind editKind
}

type editKind int

const (
	editPin editKind = iota
	editDeclare
	editRemove
)

func (e configEdit) apply(content string) (string, error) {
	switch e.kind {
	case editPin:
		return setPluginFrom(content, e.name, e.from)
	case editDeclare:
		return appendPluginDecl(content, e.name, e.from)
	case editRemove:
		return removePluginDecl(content, e.name)
	default:
		return "", internalerror.NewInternalError("unhandled configuration edit kind "+
			strconv.Itoa(int(e.kind)), nil)
	}
}

func setPluginFrom(content, name, from string) (string, error) {
	block, err := config.FindBlock(content, pluginsKey)
	if err != nil {
		return "", err
	}

	updated, rewritten := "", false
	if decl, declared := block.Find(name); declared {
		updated, rewritten = block.SetField(decl, "from", from)
	}
	if !rewritten {
		return "", internalerror.NewPreconditionError("cannot pin "+Label(name)+
			": its from: is not a line this command can rewrite — edit the declaration by hand", nil)
	}
	return updated, nil
}

func removePluginDecl(content, name string) (string, error) {
	block, err := config.FindBlock(content, pluginsKey)
	if err != nil {
		return "", err
	}
	decl, declared := block.Find(name)
	if !declared {
		return "", internalerror.NewNotFoundError("the configuration declares no "+Label(name), nil)
	}
	return block.Remove(decl), nil
}

func appendPluginDecl(content, name, from string) (string, error) {
	block, err := config.FindBlock(content, pluginsKey)
	if err != nil {
		return "", err
	}
	return block.AppendItem([]config.Field{{Key: "name", Value: name}, {Key: "from", Value: from}})
}
