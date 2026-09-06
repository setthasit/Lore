package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/fsx"
)

func readConfig(path string) (text string, cfg *config.Config, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, internalerror.NewNotFoundError("no configuration at "+path+
				" — run `lore init` to create one", err)
		}
		return "", nil, internalerror.NewInternalError("cannot read "+path, err)
	}

	parsed, err := config.Decode(bytes.NewReader(content))
	if err != nil {
		return "", nil, internalerror.NewBadRequestError("cannot parse "+path, err)
	}
	return string(content), parsed, nil
}

func writeConfig(path, updated, refusal string) error {
	if _, err := config.Decode(strings.NewReader(updated)); err != nil {
		return internalerror.NewInternalError(refusal, err)
	}
	if err := fsx.WriteAtomic(path, []byte(updated), fsx.ModeOf(path, 0o644)); err != nil {
		return internalerror.NewInternalError("cannot write "+path, err)
	}
	return nil
}
