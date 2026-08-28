package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"lore/internal/di"
	"lore/internal/errors/internalerror"
)

func newInitCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write a lore.yaml scaffold for this workspace",
		Long: "Writes a commented lore.yaml next to you, with placeholder source and\n" +
			"secret-variable names to fill in. It never touches an index: `lore sync`\n" +
			"creates that on its first run.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, *configPath)
		},
	}
}

func runInit(cmd *cobra.Command, configPath string) error {
	// O_EXCL is the refusal: checking for the file first and writing after
	// would overwrite a configuration written in between — and this one holds
	// hand-edited workspace state, not derived data.
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return internalerror.NewPreconditionError(configPath+" already exists — edit it, or pass --config to write elsewhere", nil)
		}
		return internalerror.NewInternalError("cannot create "+configPath, err)
	}

	if _, err := file.WriteString(scaffold(workspaceName(configPath))); err != nil {
		_ = file.Close()
		return internalerror.NewInternalError("cannot write "+configPath, err)
	}
	if err := file.Close(); err != nil {
		return internalerror.NewInternalError("cannot write "+configPath, err)
	}

	out := cmd.OutOrStdout()
	printfln(out, "wrote %s", configPath)
	printfln(out, "next: set the token variables it names, export %s, then run `lore sync`", di.EmbedderKeyEnv)
	return nil
}

// workspaceName guesses the workspace's name from where its configuration is
// being written: the containing directory is the project the operator is
// standing in, which is right often enough to save an edit and obvious enough to
// fix when it is not.
func workspaceName(configPath string) string {
	dir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return "myproject"
	}
	name := filepath.Base(dir)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "myproject"
	}
	return name
}

// scaffold is the starter configuration, shaped after the example in
// 06-interfaces-and-config.md. Everything optional is commented out, so the file
// loads as written once its token variables exist, and every secret appears as
// the NAME of an environment variable — never a value.
func scaffold(workspace string) string {
	return strings.ReplaceAll(`workspace: {{WORKSPACE}}

# The index is derived data: safe to delete, rebuilt by the next lore sync.
# index_path: ~/.lore/{{WORKSPACE}}.db

# Sources say what to INGEST. All optional — keep the ones you have.
# Secrets are never stored here: *_env names an environment variable.
sources:
  github:
    token_env: LORE_GITHUB_TOKEN           # export a fine-grained read-only PAT
    # Repositories to ingest, "owner/name" — no local clone needed.
    repos:
      - acme/{{WORKSPACE}}
      # - acme/{{WORKSPACE}}-infra

# Local clones, for blame and file history only. Zero repos is a valid
# ask-only workspace.
repos: []
# repos:
#   - path: ~/dev/{{WORKSPACE}}
#     remote: github:acme/{{WORKSPACE}}

# query:                                   # optional tuning
#   event_window: 30d                      # ± window for event resolution
#   walk_depth: 3
#   top_k: 12

embedder:
  provider: openai                         # the API key comes from OPENAI_API_KEY
  model: text-embedding-3-small            # changing the model needs: lore sync --reembed

# scheduler:
#   interval: 30m
`, "{{WORKSPACE}}", workspace)
}
