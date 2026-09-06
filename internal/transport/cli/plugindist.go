package cli

import (
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/urlx"
)

const pluginsKey = "plugins"

// trustNotice is printed before anything is downloaded. An external plugin runs
// as a subprocess with the user's privileges and holds its source's token; the
// engine does not sandbox it, so the one honest mitigation is saying so at the
// moment the user chooses to install.
const trustNotice = "installing a plugin runs that author's code on this machine, with your privileges" +
	" and the tokens you give its sources"

func newPluginInstallCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "install [<name> | <coordinate>[@latest]]",
		Short: "Resolve, download, verify and pin an external plugin",
		Long: "Resolves a plugin's coordinate, downloads and verifies the artifact for this\n" +
			"os/arch, unpacks it under ~/.lore/plugins and records the version, URL and\n" +
			"digest in lore.lock. With no argument it installs every plugin lore.yaml\n" +
			"declares. Nothing else in Lore ever downloads a plugin: a sync round does\n" +
			"not, and a scheduler must not.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginInstall(cmd, args, *configPath)
		},
	}
}

func newPluginUpdateCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "update <name>[@<version>]",
		Short: "Re-resolve a plugin and rewrite its locked version, URLs and digests",
		Long: "The only command that replaces a locked digest. Without a version it moves to\n" +
			"the newest release and writes that version back into lore.yaml.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginUpdate(cmd, args[0], *configPath)
		},
	}
}

func newPluginRemoveCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Drop a plugin's declaration, its lock entry and its cached versions",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginRemove(cmd, args[0], *configPath)
		},
	}
}

func newPluginVerifyCommand(configPath *string, reg *registry.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <name>",
		Short: "Re-check the digest of an installed plugin and report it",
		Long: "Re-hashes the installed binary and compares it with the digest recorded when it\n" +
			"was installed, so a cached binary rewritten after installation is caught. It\n" +
			"reports the exact binary that will run.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginVerify(cmd, args[0], *configPath, reg)
		},
	}
}

// pluginWorkspace is the configuration, its lockfile and the text of the file
// itself. The text is kept because lore.yaml is hand-written: a YAML round trip
// would reflow it and drop its comments, so every edit is a splice.
type pluginWorkspace struct {
	path    string
	dir     string
	content string
	config  *config.Config
	lock    *plugindist.Lock
	store   *plugindist.Store
}

func openPluginWorkspace(configPath string) (*pluginWorkspace, error) {
	content, parsed, err := readConfig(configPath)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(configPath)
	lock, err := plugindist.LoadLock(dir)
	if err != nil {
		return nil, err
	}
	store, err := plugindist.DefaultStore()
	if err != nil {
		return nil, err
	}
	return &pluginWorkspace{
		path: configPath, dir: dir, content: content,
		config: parsed, lock: lock, store: store,
	}, nil
}

func (w *pluginWorkspace) declaration(name string) (config.PluginDecl, bool) {
	for _, decl := range w.config.Plugins {
		if decl.Name == name {
			return decl, true
		}
	}
	return config.PluginDecl{}, false
}

func (w *pluginWorkspace) mustDeclare(name string) (config.PluginDecl, error) {
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

func (w *pluginWorkspace) declaredNames() []string {
	names := make([]string, 0, len(w.config.Plugins))
	for _, decl := range w.config.Plugins {
		names = append(names, decl.Name)
	}
	return names
}

func runPluginInstall(cmd *cobra.Command, args []string, configPath string) error {
	workspace, err := openPluginWorkspace(configPath)
	if err != nil {
		return err
	}

	requests, edits, err := installRequests(workspace, args)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		printfln(cmd.OutOrStdout(), "%s declares no plugins: — nothing to install", configPath)
		return nil
	}

	out := cmd.OutOrStdout()
	printfln(out, "%s", trustNotice)

	// Everything is installed before anything is written: an install that
	// aborts leaves lore.lock and lore.yaml exactly as they were.
	installer := plugindist.NewInstaller(workspace.store)
	results := make([]plugindist.Result, 0, len(requests))
	for _, request := range requests {
		pinned, err := installer.Pin(cmd.Context(), request.Coordinate)
		if err != nil {
			return err
		}
		if pinned.From != request.Coordinate.From {
			// @latest was an argument, so the version it resolved to goes back
			// into the file: the next machine must run the same code.
			edits = append(edits, configEdit{name: pinned.Name, from: pinned.From, kind: editPin})
		}
		request.Coordinate = pinned

		result, err := installer.Install(cmd.Context(), request, workspace.lock)
		if err != nil {
			return err
		}
		results = append(results, result)
	}

	if err := workspace.commitPins(edits, anyPinned(results)); err != nil {
		return err
	}
	for _, result := range results {
		renderInstall(out, result)
	}
	return nil
}

// installRequests turns the argument into the plugins to install, plus the
// edits lore.yaml needs once they succeed.
func installRequests(w *pluginWorkspace, args []string) ([]plugindist.Request, []configEdit, error) {
	if len(args) == 0 {
		requests := make([]plugindist.Request, 0, len(w.config.Plugins))
		for _, decl := range w.config.Plugins {
			coord, err := plugindist.Resolve(w.dir, decl)
			if err != nil {
				return nil, nil, err
			}
			requests = append(requests, plugindist.Request{Coordinate: coord})
		}
		return requests, nil, nil
	}

	target, version := splitInstallArgument(args[0])
	if !looksLikeCoordinate(target) {
		decl, err := w.mustDeclare(target)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case version == plugindist.LatestVersion:
			decl.From = coordinateAt(decl.From, version)
		case version != "":
			return nil, nil, internalerror.NewBadRequestError("install pins what lore.yaml declares; to move "+
				target+" to "+version+" run: lore plugin update "+target+"@"+version, nil)
		}

		coord, err := plugindist.ResolveInstall(w.dir, decl)
		if err != nil {
			return nil, nil, err
		}
		return []plugindist.Request{{Coordinate: coord}}, nil, nil
	}

	name, err := nameForCoordinate(target)
	if err != nil {
		return nil, nil, err
	}
	from := target
	if version != "" {
		from = target + "@" + version
	}

	decl, declared := w.declaration(name)
	coord, err := plugindist.ResolveInstall(w.dir, config.PluginDecl{Name: name, From: from, PubKey: decl.PubKey})
	if err != nil {
		return nil, nil, err
	}
	if declared && decl.From != coord.From && version != plugindist.LatestVersion {
		return nil, nil, internalerror.NewBadRequestError(w.path+" declares "+name+" from "+
			urlx.RedactIfUserinfo(decl.From)+", not "+coord.SafeFrom()+
			" — edit the declaration, or run: lore plugin update "+name, nil)
	}

	edits := []configEdit(nil)
	if !declared {
		edits = append(edits, configEdit{name: name, from: coord.From, kind: editDeclare})
	}
	return []plugindist.Request{{Coordinate: coord}}, edits, nil
}

func runPluginUpdate(cmd *cobra.Command, argument, configPath string) error {
	workspace, err := openPluginWorkspace(configPath)
	if err != nil {
		return err
	}

	name, version := splitInstallArgument(argument)
	decl, err := workspace.mustDeclare(name)
	if err != nil {
		return err
	}
	coord, err := plugindist.ResolveInstall(workspace.dir, decl)
	if err != nil {
		return err
	}
	if coord.Origin == plugindist.OriginLocal {
		return internalerror.NewBadRequestError(pluginLabel(name)+" is a local path, which is never pinned:"+
			" rebuild it in place instead", nil)
	}

	switch {
	case version != "" && version != plugindist.LatestVersion:
		if coord, err = coord.AtVersion(version); err != nil {
			return err
		}
	case coord.Origin == plugindist.OriginGitHub:
		// Update with no version means "the newest release", which is the only
		// other place @latest is legal.
		if coord, err = coord.AtVersion(plugindist.LatestVersion); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	printfln(out, "%s", trustNotice)

	installer := plugindist.NewInstaller(workspace.store)
	pinned, err := installer.Pin(cmd.Context(), coord)
	if err != nil {
		return err
	}

	result, err := installer.Install(cmd.Context(), plugindist.Request{Coordinate: pinned, Rewrite: true}, workspace.lock)
	if err != nil {
		return err
	}

	edits := []configEdit(nil)
	if pinned.From != decl.From {
		edits = append(edits, configEdit{name: name, from: pinned.From, kind: editPin})
	}
	if err := workspace.commitPins(edits, true); err != nil {
		return err
	}
	renderInstall(out, result)
	return nil
}

func runPluginRemove(cmd *cobra.Command, name, configPath string) error {
	workspace, err := openPluginWorkspace(configPath)
	if err != nil {
		return err
	}
	if _, err := workspace.mustDeclare(name); err != nil {
		return err
	}
	if _, err := config.FindBlock(workspace.content, pluginsKey); err != nil {
		return err
	}
	if used := workspace.config.InstancesUsing(name); len(used) > 0 {
		return internalerror.NewPreconditionError(pluginLabel(name)+" is still used by "+strings.Join(used, ", ")+
			" — remove those first, or the next `lore sync` has nothing to build them from", nil)
	}

	versions, err := workspace.store.Remove(name)
	if err != nil {
		return err
	}
	unlocked := workspace.lock.Remove(name)
	refusal := "removing " + pluginLabel(name) + " would leave " + workspace.path + " unreadable, so it is unchanged"
	if err := workspace.commit([]configEdit{{name: name, kind: editRemove}}, refusal, unlocked); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printfln(out, "removed %s from %s", pluginLabel(name), workspace.path)
	if unlocked {
		printfln(out, "  dropped its entry from %s", plugindist.LockFileName)
	}
	printfln(out, "  deleted %s from the plugin cache", plural(versions, "cached version", "cached versions"))
	return nil
}

func runPluginVerify(cmd *cobra.Command, name, configPath string, reg *registry.Registry) error {
	workspace, err := openPluginWorkspace(configPath)
	if err != nil {
		return err
	}
	decl, err := workspace.mustDeclare(name)
	if err != nil {
		return err
	}
	coord, err := plugindist.Resolve(workspace.dir, decl)
	if err != nil {
		return err
	}

	// Locate re-hashes the binary on disk and refuses on a mismatch, so
	// reaching this line is itself the digest's verification.
	report, err := workspace.store.Locate(name, coord, workspace.lock)
	if err != nil {
		return err
	}

	// The conformance suite sits on top of this report rather than inside it:
	// running it means opening the binary over the plugin protocol, which is
	// another package's business, and it needs the verified path this report
	// already carries.
	out := cmd.OutOrStdout()
	renderVerify(out, report)

	// The binary under test is lent the Host a running plugin gets, from the
	// registry the binary was assembled with: certifying it under different
	// conditions would certify something else.
	return certify(out, name, report.Binary, reg.Host(name))
}

func renderInstall(out io.Writer, result plugindist.Result) {
	if result.Origin == plugindist.OriginLocal {
		printfln(out, "%s runs %s in place", pluginLabel(result.Name), result.Binary)
		printfln(out, "  warning: %s", result.Warning)
		return
	}

	printfln(out, "installed %s %s for %s", pluginLabel(result.Name), result.Version, result.Platform)
	printfln(out, "  binary:  %s", result.Binary)
	printfln(out, "  digest:  %s", result.ArtifactDigest)
	switch {
	case result.Locked:
		printfln(out, "  matches the digest %s already pinned for %s", plugindist.LockFileName, result.Platform)
	case result.Trust:
		printfln(out, "  pinned in %s: nothing but the download itself vouched for these bytes", plugindist.LockFileName)
	default:
		printfln(out, "  pinned in %s", plugindist.LockFileName)
	}
	if result.Signed {
		printfln(out, "  signature: verified")
	}
}

func renderVerify(out io.Writer, report plugindist.Report) {
	if report.Origin == plugindist.OriginLocal {
		printfln(out, "%s runs %s in place — unpinned, unlocked, undigested", pluginLabel(report.Name), report.Binary)
		printfln(out, "  warning: %s", report.Warning)
		return
	}

	printfln(out, "%s %s for %s", pluginLabel(report.Name), report.Version, report.Platform)
	printfln(out, "  binary:  %s", report.Binary)
	printfln(out, "  digest:  %s (re-checked now)", report.BinaryDigest)
	printfln(out, "  locked:  %s", report.LockedDigest)
	printfln(out, "  from:    %s", urlx.RedactIfUserinfo(report.LockedURL))
	if report.Manifest {
		printfln(out, "  manifest: cached beside the binary")
		return
	}
	printfln(out, "  manifest: not cached — the handshake reads it from the binary anyway")
}

func (w *pluginWorkspace) commit(edits []configEdit, refusal string, lockChanged bool) error {
	if len(edits) > 0 {
		updated := w.content
		for _, edit := range edits {
			applied, err := edit.apply(updated)
			if err != nil {
				return err
			}
			updated = applied
		}
		if err := writeConfig(w.path, updated, refusal); err != nil {
			return err
		}
		w.content = updated
	}

	if lockChanged {
		return w.lock.Save(w.dir)
	}
	return nil
}

func (w *pluginWorkspace) commitPins(edits []configEdit, lockChanged bool) error {
	return w.commit(edits, "the pinned coordinates do not fit "+w.path+", which is unchanged", lockChanged)
}

func anyPinned(results []plugindist.Result) bool {
	for _, result := range results {
		if result.Pinned {
			return true
		}
	}
	return false
}

func splitInstallArgument(argument string) (target, version string) {
	argument = strings.TrimSpace(argument)
	versionSeparator := strings.LastIndex(argument, "@")
	if versionSeparator <= 0 || versionSeparator < strings.LastIndex(argument, "/") {
		return argument, ""
	}
	return argument[:versionSeparator], argument[versionSeparator+1:]
}

func coordinateAt(from, version string) string {
	target, _ := splitInstallArgument(from)
	return target + "@" + version
}

// looksLikeCoordinate separates `lore plugin install linear` from
// `lore plugin install github.com/jdoe/lore-linear@v0.3.1`: a short name is a
// single token, and every coordinate shape carries a separator.
func looksLikeCoordinate(target string) bool {
	return strings.Contains(target, "/") || strings.Contains(target, `\`) || strings.Contains(target, ":")
}

// nameForCoordinate derives the short name a coordinate argument gets declared
// under. Only a repository coordinate carries one: the convention is
// lore-<name>, and a plugin repository that ignores it is declared by hand
// rather than named by a guess.
func nameForCoordinate(target string) (string, error) {
	repository, isGitHub := strings.CutPrefix(target, "github.com/")
	segments := strings.Split(repository, "/")
	if !isGitHub || len(segments) != 2 || segments[1] == "" {
		return "", internalerror.NewBadRequestError("install cannot derive a name for "+urlx.RedactIfUserinfo(target)+
			" — declare it under plugins: in lore.yaml with the name every `use:` will refer to,"+
			" then run: lore plugin install <name>", nil)
	}
	return strings.TrimPrefix(segments[1], "lore-"), nil
}

func pluginLabel(name string) string {
	return pluginsKey + "[" + name + "]"
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
		return "", internalerror.NewPreconditionError("cannot pin "+pluginLabel(name)+
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
		return "", internalerror.NewNotFoundError("the configuration declares no "+pluginLabel(name), nil)
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
