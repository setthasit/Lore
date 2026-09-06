package plugindist

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/urlx"
)

// APIBaseEnv points the resolver at another GitHub API, which is what a
// GitHub Enterprise installation needs and what a test uses instead of the
// network.
const APIBaseEnv = "LORE_GITHUB_API"

const defaultAPIBase = "https://api.github.com"

// ChecksumsAsset is the second half of the goreleaser convention: one
// `<sha256>  <filename>` line per archive, and the only place a first install
// can learn a digest it has nothing to compare against yet.
const ChecksumsAsset = "checksums.txt"

// Byte caps on everything fetched. A supply chain that streams without a limit
// is a memory bomb waiting for a hostile mirror.
const (
	maxArtifactBytes = 256 << 20
	MaxMetadataBytes = 4 << 20
	maxSignatureSize = 64 << 10
)

const maxRedirects = 10

func DefaultAPIBase() string {
	if base := strings.TrimSpace(os.Getenv(APIBaseEnv)); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return defaultAPIBase
}

// release is the part of a GitHub release this package reads. The tag is read
// back because @latest is resolved through it, and the asset list is read to
// report what a release actually published when the expected name is missing.
type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type fetcher struct {
	client  *http.Client
	apiBase string
}

func (f fetcher) releaseByTag(ctx context.Context, c Coordinate) (release, error) {
	return f.release(ctx, c, "/releases/tags/"+url.PathEscape(c.Version), "tag "+c.Version)
}

func (f fetcher) latestRelease(ctx context.Context, c Coordinate) (release, error) {
	return f.release(ctx, c, "/releases/latest", "the latest release")
}

func (f fetcher) release(ctx context.Context, c Coordinate, endpoint, what string) (release, error) {
	target := f.apiBase + "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + endpoint

	body, err := BoundedGet(ctx, f.client, target, MaxMetadataBytes)
	if err != nil {
		return release{}, resolveFailure(c, "reading "+what+" of github.com/"+c.Owner+"/"+c.Repo, err)
	}

	var parsed release
	if err := json.Unmarshal(body, &parsed); err != nil {
		return release{}, resolveFailure(c, "parsing "+what+" of github.com/"+c.Owner+"/"+c.Repo, err)
	}
	return parsed, nil
}

// asset finds the artifact by the name the convention says it must have. The
// failure names both what was looked for and what the release holds, because
// the fix is always in the publisher's release pipeline and the reader needs to
// see the difference.
func (r release) asset(c Coordinate, want string) (string, error) {
	for _, asset := range r.Assets {
		if asset.Name == want {
			return asset.URL, nil
		}
	}

	held := "nothing"
	if names := r.assetNames(); len(names) > 0 {
		held = strings.Join(names, ", ")
	}
	return "", internalerror.NewPreconditionError(Label(c.Name)+": the "+c.Version+" release of github.com/"+
		c.Owner+"/"+c.Repo+" publishes no "+want+" — it has: "+held, nil)
}

func (r release) assetNames() []string {
	names := make([]string, 0, len(r.Assets))
	for _, asset := range r.Assets {
		names = append(names, asset.Name)
	}
	return names
}

func BoundedGet(ctx context.Context, client *http.Client, target string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, internalerror.NewBadRequestError("cannot request that URL", err)
	}
	safe := urlx.Redact(request.URL)
	if request.URL.Scheme != "https" {
		return nil, internalerror.NewBadRequestError("refusing to fetch "+safe+
			" — plugin traffic stays on https", nil)
	}
	request.Header.Set("Accept", "*/*")

	guarded := *client
	guarded.CheckRedirect = refuseDowngrade

	response, err := guarded.Do(request)
	if err != nil {
		var refused *internalerror.Error
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, internalerror.NewPreconditionError("cannot reach "+safe, err)
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, internalerror.NewNotFoundError("nothing published at "+safe+" (404)", nil)
	case response.StatusCode != http.StatusOK:
		return nil, internalerror.NewPreconditionError(safe+" responded "+strconv.Itoa(response.StatusCode), nil)
	}

	// Reading one byte past the cap is what separates a full body from a truncated one.
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, internalerror.NewPreconditionError("cannot read "+safe, err)
	}
	if int64(len(body)) > limit {
		return nil, internalerror.NewPreconditionError(safe+" is larger than the "+
			strconv.FormatInt(limit>>20, 10)+" MiB this build will download", nil)
	}
	return body, nil
}

func refuseDowngrade(request *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return internalerror.NewPreconditionError("stopped after "+strconv.Itoa(maxRedirects)+
			" redirects at "+urlx.Redact(request.URL), nil)
	}
	if request.URL.Scheme == "https" {
		return nil
	}
	return internalerror.NewPreconditionError("refusing a redirect to "+urlx.Redact(request.URL)+
		" — plugin traffic stays on https", nil)
}

func safeTarget(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "an unreadable URL"
	}
	return urlx.Redact(parsed)
}

// resolveFailure names the coordinate and the step that failed, which together
// are the whole diagnosis for an unresolvable coordinate.
func resolveFailure(c Coordinate, step string, cause error) error {
	message := Label(c.Name) + " cannot resolve " + c.SafeFrom() + ": " + step + " failed"
	if actionable := internalerror.MessageOf(cause); actionable != "" {
		message += " — " + actionable
	}

	if internalerror.IsNotFound(cause) {
		return internalerror.NewNotFoundError(message, cause)
	}
	return internalerror.NewPreconditionError(message, cause)
}

// checksumFor reads the digest of one file out of a checksums.txt. The format
// is one `<sha256>  <filename>` line per archive; a coreutils-style `*` marks
// binary mode and says nothing about the digest.
func checksumFor(body []byte, asset string) (string, bool) {
	for line := range strings.Lines(string(body)) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 64 {
			return "", false
		}
		return "sha256:" + strings.ToLower(fields[0]), true
	}
	return "", false
}

// siblingURL names a file published beside another one, which is how a
// checksums file and its signature are reached when the artifact URL is all
// there is — a URL coordinate has no release to enumerate.
func siblingURL(artifact, name string) string {
	at := strings.LastIndex(artifact, "/")
	if at < 0 {
		return artifact + "/" + name
	}
	return artifact[:at+1] + name
}

// artifactFileName is the name the digest is recorded against in a
// checksums.txt, which for any coordinate is the last segment of its URL.
func artifactFileName(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	return path.Base(parsed.Path)
}
