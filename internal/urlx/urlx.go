package urlx

import "net/url"

// A credential in a path segment or a fragment survives; the query does not.
func Redact(target *url.URL) string {
	stripped := *target
	stripped.User, stripped.RawQuery = nil, ""
	return stripped.String()
}

// A string carrying no userinfo comes back byte-identical, so a filesystem path
// or a value that is not a URL at all is never mangled.
func RedactIfUserinfo(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	return Redact(parsed)
}
