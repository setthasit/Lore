package plugbuild

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/plugindist"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/urlx"
	"github.com/setthasit/Lore/sdk"
)

const DefaultIndexURL = "https://raw.githubusercontent.com/setthasit/lore-plugins/main/index.json"

const indexVersion = 1

const maxEntryFieldBytes = 120

type Entry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Summary    string `json:"summary"`
	Coordinate string `json:"coordinate"`
}

type document struct {
	Version int               `json:"version"`
	Plugins []json.RawMessage `json:"plugins"`
}

type Index struct {
	HTTP *http.Client
	URL  string
}

func (i Index) Fetch(ctx context.Context) (entries []Entry, skipped int, err error) {
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
			return nil, 0, internalerror.NewPreconditionError(
				"the plugin index at "+safe+" is unreachable — searching needs network access; a plugin's own "+
					"README is the other place its coordinate is written", err)
		}
		return nil, 0, internalerror.NewPreconditionError(
			"the plugin index at "+safe+" is unusable — "+internalerror.MessageOf(err), err)
	}

	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, 0, internalerror.NewPreconditionError(
			"the plugin index at "+safe+" is not a readable index", err)
	}
	if doc.Version != indexVersion {
		return nil, 0, internalerror.NewPreconditionError(
			"the plugin index at "+safe+" is version "+strconv.Itoa(doc.Version)+
				", and this build reads version "+strconv.Itoa(indexVersion), nil)
	}

	entries, skipped = usableEntries(doc.Plugins)
	return entries, skipped, nil
}

func usableEntries(published []json.RawMessage) ([]Entry, int) {
	usable := make([]Entry, 0, len(published))
	for _, raw := range published {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil || !usableEntry(e) {
			continue
		}
		usable = append(usable, e)
	}
	return usable, len(published) - len(usable)
}

func usableEntry(e Entry) bool {
	for _, field := range []string{e.Name, e.Kind, e.Summary, e.Coordinate} {
		if len(field) > maxEntryFieldBytes || !printableField(field) {
			return false
		}
	}
	return registry.ValidPluginName(e.Name) && knownKind(e.Kind) && e.Coordinate != "" &&
		!strings.HasPrefix(e.Coordinate, "-") && !strings.ContainsFunc(e.Coordinate, unicode.IsSpace)
}

// A byte the index published as invalid UTF-8 arrives here as U+FFFD.
func printableField(field string) bool {
	for _, r := range field {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func knownKind(kind string) bool {
	switch lore.Kind(kind) {
	case lore.KindSource, lore.KindProvider, lore.KindCode:
		return true
	}
	return false
}

// Match is a case-insensitive substring over name, summary and kind, in index order.
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
