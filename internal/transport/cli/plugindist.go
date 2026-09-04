package cli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
)

// pluginsKey is the top-level key an external plugin is declared under, and the
// only thing this file knows about the shape of a configuration.
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
	content, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, internalerror.NewNotFoundError("no configuration at "+configPath+
				" — run `lore init` to create one", err)
		}
		return nil, internalerror.NewInternalError("cannot read "+configPath, err)
	}

	parsed, err := config.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, internalerror.NewBadRequestError("cannot parse "+configPath, err)
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
		path: configPath, dir: dir, content: string(content),
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
			edits = append(edits, configEdit{name: pinned.Name, from: pinned.From})
		}
		request.Coordinate = pinned

		result, err := installer.Install(cmd.Context(), request, workspace.lock)
		if err != nil {
			return err
		}
		results = append(results, result)
	}

	if err := workspace.commit(edits, anyPinned(results)); err != nil {
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
			requests = append(requests, plugindist.Request{Coordinate: coord, PubKey: decl.PubKey})
		}
		return requests, nil, nil
	}

	target, version := splitInstallArgument(args[0])
	if !looksLikeCoordinate(target) {
		decl, err := w.mustDeclare(target)
		if err != nil {
			return nil, nil, err
		}
		from := decl.From
		switch {
		case version == plugindist.LatestVersion:
			from = coordinateAt(decl.From, version)
		case version != "":
			return nil, nil, internalerror.NewBadRequestError("install pins what lore.yaml declares; to move "+
				target+" to "+version+" run: lore plugin update "+target+"@"+version, nil)
		}

		coord, err := plugindist.ResolveInstall(w.dir, target, from)
		if err != nil {
			return nil, nil, err
		}
		return []plugindist.Request{{Coordinate: coord, PubKey: decl.PubKey}}, nil, nil
	}

	name, err := nameForCoordinate(target)
	if err != nil {
		return nil, nil, err
	}
	from := target
	if version != "" {
		from = target + "@" + version
	}

	coord, err := plugindist.ResolveInstall(w.dir, name, from)
	if err != nil {
		return nil, nil, err
	}
	decl, declared := w.declaration(name)
	if declared && decl.From != coord.From && version != plugindist.LatestVersion {
		return nil, nil, internalerror.NewBadRequestError(w.path+" declares "+name+" from "+decl.From+
			", not "+coord.From+" — edit the declaration, or run: lore plugin update "+name, nil)
	}

	edits := []configEdit(nil)
	if !declared {
		edits = append(edits, configEdit{name: name, from: coord.From, add: true})
	}
	return []plugindist.Request{{Coordinate: coord, PubKey: decl.PubKey}}, edits, nil
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
	coord, err := plugindist.ResolveInstall(workspace.dir, name, decl.From)
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

	result, err := installer.Install(cmd.Context(), plugindist.Request{
		Coordinate: pinned, PubKey: decl.PubKey, Rewrite: true,
	}, workspace.lock)
	if err != nil {
		return err
	}

	edits := []configEdit(nil)
	if pinned.From != decl.From {
		edits = append(edits, configEdit{name: name, from: pinned.From})
	}
	if err := workspace.commit(edits, true); err != nil {
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
	if used := instancesUsing(workspace.config, name); len(used) > 0 {
		return internalerror.NewPreconditionError(pluginLabel(name)+" is still used by "+strings.Join(used, ", ")+
			" — remove those first, or the next `lore sync` has nothing to build them from", nil)
	}

	updated, err := removePluginDecl(workspace.content, name)
	if err != nil {
		return err
	}
	if _, err := config.Decode(strings.NewReader(updated)); err != nil {
		return internalerror.NewInternalError("removing "+pluginLabel(name)+" would leave "+workspace.path+
			" unreadable, so it is unchanged", err)
	}

	versions, err := workspace.store.Remove(name)
	if err != nil {
		return err
	}
	unlocked := workspace.lock.Remove(name)
	if err := replaceFile(workspace.path, updated); err != nil {
		return err
	}
	if unlocked {
		if err := workspace.lock.Save(workspace.dir); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	printfln(out, "removed %s from %s", pluginLabel(name), workspace.path)
	if unlocked {
		printfln(out, "  dropped its entry from %s", plugindist.LockFileName)
	}
	printfln(out, "  deleted %s from the plugin cache", pluralize(versions, "cached version", "cached versions"))
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
	printfln(out, "  from:    %s", report.LockedURL)
	if report.Manifest {
		printfln(out, "  manifest: cached beside the binary")
		return
	}
	printfln(out, "  manifest: not cached — the handshake reads it from the binary anyway")
}

// commit writes the configuration and the lockfile, in that order and only
// after every install succeeded. The configuration is decoded before it is
// written, so a splice that would no longer load is refused while the file on
// disk is still the old one.
func (w *pluginWorkspace) commit(edits []configEdit, lockChanged bool) error {
	if len(edits) > 0 {
		updated := w.content
		for _, edit := range edits {
			applied, err := edit.apply(updated)
			if err != nil {
				return err
			}
			updated = applied
		}
		if _, err := config.Decode(strings.NewReader(updated)); err != nil {
			return internalerror.NewInternalError("the pinned coordinates do not fit "+w.path+
				", which is unchanged", err)
		}
		if err := replaceFile(w.path, updated); err != nil {
			return err
		}
		w.content = updated
	}

	if lockChanged {
		return w.lock.Save(w.dir)
	}
	return nil
}

func anyPinned(results []plugindist.Result) bool {
	for _, result := range results {
		if result.Pinned {
			return true
		}
	}
	return false
}

// splitInstallArgument separates the version an argument may carry. A local
// path is never versioned, and splitting on the last "@" leaves a URL's
// userinfo alone.
func splitInstallArgument(argument string) (target, version string) {
	argument = strings.TrimSpace(argument)
	at := strings.LastIndex(argument, "@")
	if at <= 0 {
		return argument, ""
	}
	return argument[:at], argument[at+1:]
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
		return "", internalerror.NewBadRequestError("install cannot derive a name for "+target+
			" — declare it under plugins: in lore.yaml with the name every `use:` will refer to,"+
			" then run: lore plugin install <name>", nil)
	}
	return strings.TrimPrefix(segments[1], "lore-"), nil
}

// instancesUsing reports the configured instances that would be left without a
// plugin, so a removal never knowingly writes a configuration that fails to load.
func instancesUsing(cfg *config.Config, name string) []string {
	used := []string(nil)
	for section, instances := range map[string][]config.Instance{
		"sources": cfg.Sources, "providers": cfg.Providers,
	} {
		for _, instance := range instances {
			if instance.Use == name {
				used = append(used, section+"["+instance.Ident()+"]")
			}
		}
	}
	for _, repo := range cfg.Repos {
		if repo.Use == name {
			used = append(used, "repos["+repo.Path+"]")
		}
	}
	slices.Sort(used)
	return used
}

func pluginLabel(name string) string {
	return pluginsKey + "[" + name + "]"
}

// configEdit is a change to the plugins: block of lore.yaml. There are only
// two: pin a declaration's version, and declare a plugin an argument named.
type configEdit struct {
	name string
	from string
	add  bool
}

func (e configEdit) apply(content string) (string, error) {
	if e.add {
		return appendPluginDecl(content, e.name, e.from)
	}
	return setPluginFrom(content, e.name, e.from)
}

// pluginDecl is where one declaration sits in the text of a configuration.
type pluginDecl struct {
	name     string
	start    int // 1-based first line of the sequence item
	end      int // 1-based last line of the sequence item
	fromLine int // 1-based line carrying from:, zero when the key is absent
}

// pluginBlock is the plugins: key as it appears in the file. The nodes are read
// for their line numbers only: everything else is a splice into text the
// commands are not allowed to reflow.
type pluginBlock struct {
	found   bool
	keyLine int
	inline  bool // the key carries a value on its own line, as in `plugins: []`
	items   []pluginDecl
}

func readPluginBlock(content string) (pluginBlock, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return pluginBlock{}, internalerror.NewBadRequestError("cannot parse the configuration", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return pluginBlock{}, nil
	}

	document := root.Content[0]
	for at := 0; at+1 < len(document.Content); at += 2 {
		if document.Content[at].Value != pluginsKey {
			continue
		}

		key, value := document.Content[at], document.Content[at+1]
		block := pluginBlock{found: true, keyLine: key.Line, inline: value.Line == key.Line}
		for _, item := range value.Content {
			block.items = append(block.items, readPluginDecl(item))
		}
		return block, nil
	}
	return pluginBlock{}, nil
}

func readPluginDecl(item *yaml.Node) pluginDecl {
	decl := pluginDecl{start: item.Line, end: maxLine(item)}
	for at := 0; at+1 < len(item.Content); at += 2 {
		switch item.Content[at].Value {
		case "name":
			decl.name = item.Content[at+1].Value
		case "from":
			decl.fromLine = item.Content[at+1].Line
		}
	}
	return decl
}

// maxLine is the last line a node occupies, which for a declaration of scalars
// is the line of its last value.
func maxLine(node *yaml.Node) int {
	last := node.Line
	for _, child := range node.Content {
		if line := maxLine(child); line > last {
			last = line
		}
	}
	return last
}

func (b pluginBlock) find(name string) (pluginDecl, bool) {
	for _, item := range b.items {
		if item.name == name {
			return item, true
		}
	}
	return pluginDecl{}, false
}

// setPluginFrom rewrites one declaration's from: value in place, preserving its
// indentation and any comment after it.
func setPluginFrom(content, name, from string) (string, error) {
	block, err := readPluginBlock(content)
	if err != nil {
		return "", err
	}
	decl, found := block.find(name)
	if !found || decl.fromLine == 0 {
		return "", internalerror.NewPreconditionError("cannot pin "+pluginLabel(name)+
			": its from: is not a line this command can rewrite — edit the declaration by hand", nil)
	}

	lines := strings.SplitAfter(content, "\n")
	line := strings.TrimRight(lines[decl.fromLine-1], "\r\n")
	at := strings.Index(line, "from:")
	if at < 0 {
		return "", internalerror.NewPreconditionError("cannot pin "+pluginLabel(name)+
			": its from: is not a line this command can rewrite — edit the declaration by hand", nil)
	}

	_, comment := splitInlineComment(line[at+len("from:"):])
	replaced := line[:at] + "from: " + from
	if comment != "" {
		replaced += " " + comment
	}
	lines[decl.fromLine-1] = replaced + "\n"
	return strings.Join(lines, ""), nil
}

// removePluginDecl deletes the declaration's own lines and nothing else: a
// comment below it introduces whatever follows, so it stays.
func removePluginDecl(content, name string) (string, error) {
	block, err := readPluginBlock(content)
	if err != nil {
		return "", err
	}
	decl, found := block.find(name)
	if !found {
		return "", internalerror.NewNotFoundError("the configuration declares no "+pluginLabel(name), nil)
	}

	// The key goes with its last item: `plugins:` on its own is a null value
	// the decoder reads as no plugins, but it is a leftover nobody wrote.
	dropKey := len(block.items) == 1

	var kept strings.Builder
	for at, line := range strings.SplitAfter(content, "\n") {
		number := at + 1
		if number >= decl.start && number <= decl.end {
			continue
		}
		if dropKey && number == block.keyLine {
			continue
		}
		kept.WriteString(line)
	}
	return kept.String(), nil
}

// appendPluginDecl adds a declaration under plugins:, creating the block when
// the file has none.
func appendPluginDecl(content, name, from string) (string, error) {
	block, err := readPluginBlock(content)
	if err != nil {
		return "", err
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	if !block.found {
		return content + pluginsKey + ":\n" + declItemText(sequenceIndent, name, from), nil
	}
	if block.inline {
		return "", internalerror.NewPreconditionError(pluginsKey+": in the configuration carries an inline"+
			" value, so a declaration cannot be appended to it — rewrite it as a block sequence of items,"+
			" or add the declaration by hand", nil)
	}

	lines := strings.SplitAfter(content, "\n")
	after, indent := block.keyLine, sequenceIndent
	if len(block.items) > 0 {
		last := block.items[len(block.items)-1]
		after, indent = last.end, declIndent(lines[last.start-1])
	}

	var updated strings.Builder
	for at, line := range lines {
		updated.WriteString(line)
		if at+1 == after {
			updated.WriteString(declItemText(indent, name, from))
		}
	}
	return updated.String(), nil
}

// declItemText renders a declaration as one sequence item at the block's own
// indentation, name first: a declaration is read by the name every `use:`
// refers to, so that is the line the reader's eye lands on.
func declItemText(indent, name, from string) string {
	return indent + "- name: " + name + "\n" + indent + "  from: " + from + "\n"
}

// declIndent is the indentation an existing item sits at, which a new one has
// to match: YAML reads an item indented differently from its siblings as a
// nested sequence.
func declIndent(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}
