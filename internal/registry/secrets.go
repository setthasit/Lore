package registry

import (
	"fmt"
	"os"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

// Plugins never read the environment; the host resolves their secrets here on
// their behalf.
func resolveSecrets(manifest lore.Manifest, in Instance) (map[string]string, error) {
	if len(manifest.Secrets) == 0 {
		return nil, nil
	}

	secrets := make(map[string]string, len(manifest.Secrets))
	for _, s := range manifest.Secrets {
		name := s.DefaultEnv
		if declared, set := in.With[s.ConfigField]; set {
			named, ok := declared.(string)
			if !ok || named == "" {
				return nil, internalerror.NewBadRequestError(fmt.Sprintf(
					"%s.with.%s must name an environment variable", in.Field, s.ConfigField), nil)
			}
			name = named
		}
		if name == "" {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s must name the environment variable holding the %s", in.Field, s.ConfigField, secretDoc(s)), nil)
		}
		if !envPattern.MatchString(name) {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s must be an environment variable name, got %s", in.Field, s.ConfigField, name), nil)
		}

		value := os.Getenv(name)
		if value == "" {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s names %s, but that environment variable is not set", in.Field, s.ConfigField, name), nil)
		}
		secrets[s.Key] = value
	}
	return secrets, nil
}

func secretDoc(s lore.Secret) string {
	if s.Doc != "" {
		return s.Doc
	}
	return s.Key
}
