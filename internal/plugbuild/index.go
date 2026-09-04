package plugbuild

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// DefaultIndexURL is the plugin index: a JSON file in a git repository, read
// over HTTP at the pinned branch. There is no disk cache and no ranking — the
// index is one small file, and an ecosystem large enough to need either does
// not exist yet.
const DefaultIndexURL = "https://raw.githubusercontent.com/setthasit/lore-plugins/main/index.json"

// maxIndexBytes bounds what a search will read. The index is a catalogue, so a
// file orders of magnitude larger than the whole plugin ecosystem is a mistake
// or an attack, and either way not worth buffering.
const maxIndexBytes = 4 << 20

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

// Index reads the plugin index. The client is injected because the index is the
// one thing in this package that talks to the network.
type Index struct {
	HTTP *http.Client
	URL  string
}

// Fetch returns every entry in the index. An empty index is not an error: until
// an ecosystem exists that is the honest state of the world, and the caller
// says so in words rather than printing nothing.
func (i Index) Fetch(ctx context.Context) ([]Entry, error) {
	url := i.URL
	if url == "" {
		url = DefaultIndexURL
	}
	client := i.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, internalerror.NewInternalError("the plugin index URL "+url+" is not usable", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+url+" is unreachable — searching needs network access; a plugin's own "+
				"README is the other place its coordinate is written", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+url+" answered HTTP "+strconv.Itoa(response.StatusCode), nil)
	}

	var doc document
	if err := json.NewDecoder(io.LimitReader(response.Body, maxIndexBytes)).Decode(&doc); err != nil {
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+url+" is not a readable index", err)
	}
	if doc.Version != indexVersion {
		return nil, internalerror.NewPreconditionError(
			"the plugin index at "+url+" is version "+strconv.Itoa(doc.Version)+
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
