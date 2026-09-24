package registry

import (
	"fmt"
	"os"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

func (r *Registry) resolveSecrets(manifest lore.Manifest, in Instance, origin string) (map[string]string, error) {
	if len(manifest.Secrets) == 0 {
		return nil, nil
	}

	secrets := make(map[string]string, len(manifest.Secrets))
	compiledIn := origin == OriginBuiltin
	for _, s := range manifest.Secrets {
		var name string
		var operatorNamed bool
		where := in.Field
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
			where += ".with." + s.ConfigField
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
				"%s.with.%s must be an environment variable name: %s", in.Field, s.ConfigField, EnvNameRule), nil)
		}

		value := os.Getenv(name)
		if strings.TrimSpace(value) == "" {
			if s.Optional && !operatorNamed {
				continue
			}
			if in.ImpliedByRole {
				return nil, internalerror.NewBadRequestError(fmt.Sprintf(
					"%s names plugin %q, whose default variable %s is not set or is blank; export it, or %s",
					in.Field, in.Use, name, declareProvider(in, s)), nil)
			}
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s.with.%s names %s, but that environment variable is not set or is blank", in.Field, s.ConfigField, name), nil)
		}
		r.sink.Record(name+" ("+where+")", value)
		secrets[s.Key] = value
	}
	return secrets, nil
}

func unnamedSecret(in Instance, s lore.Secret, compiledIn bool) error {
	var message string
	if in.ImpliedByRole {
		message = fmt.Sprintf(
			"%s names plugin %q, which no providers: entry declares, so nothing can name the environment variable holding the %s",
			in.Field, in.Use, secretDoc(s))
	} else {
		message = fmt.Sprintf(
			"%s.with.%s must name the environment variable holding the %s", in.Field, s.ConfigField, secretDoc(s))
	}
	if !compiledIn && s.DefaultEnv != "" {
		message += "; a plugin installed from outside the binary cannot choose it"
	}
	if in.ImpliedByRole {
		message += "; " + declareProvider(in, s)
	}
	return internalerror.NewBadRequestError(message, nil)
}

func declareProvider(in Instance, s lore.Secret) string {
	return fmt.Sprintf("declare `providers: [{id: %s, use: %s, with: {%s: YOUR_VARIABLE}}]` and keep %s naming %q",
		in.Use, in.Use, s.ConfigField, in.Field, in.Use)
}

func secretDoc(s lore.Secret) string {
	if s.Doc != "" {
		return s.Doc
	}
	return s.Key
}
