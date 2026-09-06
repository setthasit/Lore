package plugbuild

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/urlx"
)

// DefaultIndexURL is the plugin index: a JSON file in a git repository, read
// over HTTP at the pinned branch. There is no disk cache and no ranking — the
// index is one small file, and an ecosystem large enough to need either does
// not exist yet.
const DefaultIndexURL = "https://raw.githubusercontent.com/setthasit/lore-plugins/main/index.json"

// indexVersion is the schema this reader understands.
const indexVersion = 1

// Entry is one plugin in the index. Coordinate is what `lore plugin install` or
// `lore build --with` is given, so the output of a search is directly the input
// of the next command.
type Entry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Summary    string `json:"summary"`
	Coordinate string `json:"coordinate"`
}

type document struct {
	Version int     `json:"version"`
	Plugins []Entry `json:"plugins"`
}

type Index struct {
	HTTP *http.Client
	URL  string
}

// An empty index is not an error; the caller says so in words.
func (i Index) Fetch(ctx context.Context) ([]Entry, error) {
	target := i.URL
	if target == "" {
		target = DefaultIndexURL
	}
	client := i.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	safe := urlx.RedactIfUserinfo(target)

	body, err := plugindist.BoundedGet(ctx, client, target, plugindist.MaxMetadataBytes)
	if err != nil {
		if errors.As(err, new(*url.Error)) {
			return nil, internalerror.NewPreconditionError(
				"the plugin index at "+safe+" is unreachable — searching needs network access; a plugin's own "+
					"README is the other place its coordinate is written", err)
		}
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+safe+" is unusable — "+internalerror.MessageOf(err), err)
	}

	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+safe+" is not a readable index", err)
	}
	if doc.Version != indexVersion {
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+safe+" is version "+strconv.Itoa(doc.Version)+
				", and this build reads version "+strconv.Itoa(indexVersion), nil)
	}
	return doc.Plugins, nil
}

// Match returns the entries whose name, summary or kind contains the query,
// case-insensitively, in index order. There is deliberately no scoring: a
// ranking nobody can evaluate is worse than the order the index was written in.
func Match(entries []Entry, query string) []Entry {
	needle := strings.ToLower(strings.TrimSpace(query))
	matched := make([]Entry, 0, len(entries))
	for _, e := range entries {
		haystack := strings.ToLower(e.Name + "\x00" + e.Summary + "\x00" + e.Kind)
		if strings.Contains(haystack, needle) {
			matched = append(matched, e)
		}
	}
	return matched
}
