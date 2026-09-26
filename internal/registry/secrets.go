package registry

import (
	"fmt"
	"os"
	"strings"

	"github.com/setthasit/Lore/internal/envx"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

func (r *Registry) resolveSecrets(manifest lore.Manifest, in Instance, origin string, vars map[string][]string) (map[string]string, error) {
	if len(manifest.Secrets) == 0 {
		return nil, nil
	}

	secrets := make(map[string]string, len(manifest.Secrets))
	compiledIn := origin == OriginBuiltin
	for _, s := range manifest.Secrets {
		if held, set := in.With[s.Key]; set {
			field := in.secretField(s.Key)
			value, ok := held.(string)
			if ok && strings.TrimSpace(value) == "" && len(vars[s.Key]) > 0 {
				return nil, internalerror.NewBadRequestError(fmt.Sprintf(
					"%s expands %s, but it is blank", field, strings.Join(vars[s.Key], ", ")), nil)
			}
			if !ok || strings.TrimSpace(value) == "" {
				return nil, internalerror.NewBadRequestError(mustHold(field, s), nil)
			}
			if len(vars[s.Key]) == 0 {
				r.sink.RecordLiteral(field, value)
			} else {
				r.sink.Record(field, value)
			}
			secrets[s.Key] = value
			continue
		}

		// An external plugin's own default would steer the host onto a variable the operator never granted.
		if !compiledIn || s.DefaultEnv == "" {
			if s.Optional {
				continue
			}
			return nil, unnamedSecret(in, s, compiledIn)
		}
		value := os.Getenv(s.DefaultEnv)
		if strings.TrimSpace(value) == "" {
			if s.Optional {
				continue
			}
			return nil, internalerror.NewBadRequestError(fmt.Sprintf(
				"%s is not set, and its default variable %s is not set or is blank; export %s, or %s",
				in.secretField(s.Key), s.DefaultEnv, s.DefaultEnv, supplySecret(in, s)), nil)
		}
		r.sink.Record(s.DefaultEnv+" ("+in.Field+")", value)
		secrets[s.Key] = value
	}
	return secrets, nil
}

func unnamedSecret(in Instance, s lore.Secret, compiledIn bool) error {
	field := in.secretField(s.Key)
	message := mustHold(field, s)
	if in.Role != "" {
		message = fmt.Sprintf("%s is not set, and no providers: entry declares plugin %q to supply it%s",
			field, in.Use, secretDoc(s))
	}
	if !compiledIn && s.DefaultEnv != "" {
		message += "; a plugin installed from outside the binary cannot choose a default"
	}
	if in.Role != "" {
		message += "; " + declareProvider(in, s)
	}
	return internalerror.NewBadRequestError(message, nil)
}

func mustHold(field string, s lore.Secret) string {
	return fmt.Sprintf("%s must hold the %s as a value or as `%s`%s",
		field, s.Key, envx.Form, secretDoc(s))
}

func supplySecret(in Instance, s lore.Secret) string {
	if in.Role != "" {
		return declareProvider(in, s)
	}
	return "set the field, as a value or as `" + envx.Form + "`"
}

func (in Instance) secretField(key string) string {
	if in.Role != "" {
		return in.Role + "." + key
	}
	return in.Field + ".with." + key
}

func declareProvider(in Instance, s lore.Secret) string {
	return fmt.Sprintf("declare `providers: [{id: %s, use: %s, with: {%s: %q}}]` and keep %s naming %q",
		in.Use, in.Use, s.Key, envx.Reference("YOUR_VARIABLE"), in.Field, in.Use)
}

func secretDoc(s lore.Secret) string {
	return doc(strings.TrimSuffix(s.Doc, "."))
}
