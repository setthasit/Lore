package registry

import (
	"fmt"
	"os"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

func resolveSecrets(manifest lore.Manifest, in Instance, origin string) (map[string]string, error) {
	if len(manifest.Secrets) == 0 {
		return nil, nil
	}

	secrets := make(map[string]string, len(manifest.Secrets))
	compiledIn := origin == OriginBuiltin
	for _, s := range manifest.Secrets {
		var name string
		var operatorNamed bool
		// An external plugin's own default would steer the host onto a variable the operator never granted.
		if compiledIn {
			name = s.DefaultEnv
		}
		if declared, set := in.With[s.ConfigField]; set {
			named, ok := declared.(string)
			if !ok || named == "" {
				return nil, internalerror.NewBadRequestError(fmt.Sprintf(
					"%s.with.%s must name an environment variable", in.Field, s.ConfigField), nil)
			}
			name, operatorNamed = named, true
		}
		if name == "" {
			if s.Optional {
				continue
			}
			return nil, unnamedSecret(in, s, compiledIn)
		}
		// The value is withheld: an operator who pastes a credential here must not see it echoed back.
		if !envPattern.MatchString(name) {
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s must be an environment variable name: upper-case letters, digits and underscores, not starting with a digit",
				in.Field, s.ConfigField), nil)
		}

		value := os.Getenv(name)
		if value == "" {
			if s.Optional && !operatorNamed {
				continue
			}
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s names %s, but that environment variable is not set", in.Field, s.ConfigField, name), nil)
		}
		secrets[s.Key] = value
	}
	return secrets, nil
}

func unnamedSecret(in Instance, s lore.Secret, compiledIn bool) error {
	message := fmt.Sprintf(
		"%s.with.%s must name the environment variable holding the %s", in.Field, s.ConfigField, secretDoc(s))
	if !compiledIn && s.DefaultEnv != "" {
		message += "; a plugin installed from outside the binary cannot choose it"
	}
	return internalerror.NewBadRequestError(message, nil)
}

func secretDoc(s lore.Secret) string {
	if s.Doc != "" {
		return s.Doc
	}
	return s.Key
}
