package cli

import (
	"io"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/entities"
)

// Stamped by the release build with -ldflags "-X". A plain `go build` leaves
// them empty and the values below come from the binary's own build info, so an
// unstamped binary still identifies itself.
var (
	version   string
	commit    string
	buildDate string
)

const (
	unknownStamp = "unknown"
	develVersion = "devel"
)

type buildStamp struct {
	Version   string
	Commit    string
	Date      string
	GoVersion string
	Platform  string
}

func stamp() buildStamp {
	s := buildStamp{
		Version:   version,
		Commit:    commit,
		Date:      buildDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	info, ok := debug.ReadBuildInfo()
	if ok {
		s.fillFromBuildInfo(info)
	}
	if s.Version == "" {
		s.Version = develVersion
	}
	if s.Commit == "" {
		s.Commit = unknownStamp
	}
	if s.Date == "" {
		s.Date = unknownStamp
	}
	return s
}

// A module built by `go install path@version` carries its version; a build from
// a working tree carries the VCS stamp instead, dirty flag included.
func (s *buildStamp) fillFromBuildInfo(info *debug.BuildInfo) {
	if s.Version == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		s.Version = info.Main.Version
	}

	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if s.Commit == "" {
				s.Commit = shortSHA(setting.Value)
			}
		case "vcs.time":
			if s.Date == "" {
				s.Date = setting.Value
			}
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if modified && s.Commit != "" {
		s.Commit += "-dirty"
	}
}

func shortSHA(sha string) string {
	const short = 7
	if len(sha) <= short {
		return sha
	}
	return sha[:short]
}

// A missing or unopenable workspace is reported, never fatal: `lore --version`
// is the first thing a bug report runs, including on a machine with no config.
func runVersion(cmd *cobra.Command, resolve Resolver, configPath string) error {
	out := cmd.OutOrStdout()
	renderStamp(out, stamp())

	rt, stop, err := resolve(cmd.Context(), configPath)
	if err != nil {
		printfln(out, "workspace: unavailable — %s", actionableMessage(err))
		return nil
	}
	defer func() { _ = stop() }()

	printfln(out, "workspace: %s — %s", rt.Config.Workspace, rt.Config.IndexPath)
	identity, err := rt.Status.EmbedderIdentity(cmd.Context())
	renderEmbedder(out, identity, err)
	return nil
}

func renderStamp(w io.Writer, s buildStamp) {
	printfln(w, "lore %s", s.Version)
	printfln(w, "build:     %s (%s) %s %s", s.Commit, s.Date, s.GoVersion, s.Platform)
}

func renderEmbedder(w io.Writer, identity entities.EmbedderIdentity, err error) {
	if err != nil {
		printfln(w, "embedder:  unavailable — %s", actionableMessage(err))
		return
	}

	printfln(w, "embedder:  %s", identity.Configured)
	switch identity.Indexed {
	case "":
		printfln(w, "index:     no vectors yet — run `lore sync`")
	case identity.Configured:
		printfln(w, "index:     %s", identity.Indexed)
	default:
		printfln(w, "index:     %s — mismatch; run `lore sync --reembed`", identity.Indexed)
	}
}
