package pack

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
)

// MarketplaceEntry is one plugin of an owner marketplace: its name and its
// dir relative to the marketplace root ("./<name dir>/<version>").
type MarketplaceEntry struct {
	Name   string
	Source string
}

// marketplaceDocument is the wire shape of an owner marketplace.
type marketplaceDocument struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"plugins"`
}

// RenderMarketplace renders the owner marketplace document
// (`<store>/synth/<owner>/.claude-plugin/marketplace.json`, decision F3): a
// pure function of the marketplace name and its entries, sorted by name. The
// name — the owner, or `<owner>-verger` when a host already has a foreign
// `<owner>` marketplace — and every entry name are plugin-id parts; every
// source is a dir strictly inside the marketplace root (`./…`, never `..`),
// which hosts require.
func RenderMarketplace(name string, entries []MarketplaceEntry) ([]byte, error) {
	if err := validIDPart(name); err != nil {
		return nil, &RenderError{Component: marketplaceFile, Cause: fmt.Errorf("the marketplace name %w", err)}
	}

	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b MarketplaceEntry) int { return cmp.Compare(a.Name, b.Name) })

	plugins := make([]any, 0, len(sorted))

	for i, entry := range sorted {
		if err := validMarketplaceEntry(entry); err != nil {
			return nil, &RenderError{Component: marketplaceFile, Cause: err}
		}

		if i > 0 && sorted[i-1].Name == entry.Name {
			return nil, &RenderError{Component: marketplaceFile, Cause: fmt.Errorf("plugin %q is listed twice", entry.Name)}
		}

		plugins = append(plugins, map[string]any{keyName: entry.Name, "source": entry.Source})
	}

	data, err := encodeJSON(map[string]any{
		keyName:   name,
		"owner":   map[string]any{keyName: name},
		"plugins": plugins,
	})
	if err != nil {
		return nil, &RenderError{Component: marketplaceFile, Cause: err}
	}

	return data, nil
}

// ParseMarketplace reads an owner marketplace document back: its name and
// its entries, sorted by name. A document RenderMarketplace would not write
// is an error.
func ParseMarketplace(data []byte) (string, []MarketplaceEntry, error) {
	var doc marketplaceDocument

	if err := json.Unmarshal(data, &doc); err != nil {
		return "", nil, &RenderError{Component: marketplaceFile, Cause: err}
	}

	if err := validIDPart(doc.Name); err != nil {
		return "", nil, &RenderError{Component: marketplaceFile, Cause: fmt.Errorf("the marketplace name %w", err)}
	}

	entries := make([]MarketplaceEntry, 0, len(doc.Plugins))

	for _, plugin := range doc.Plugins {
		entry := MarketplaceEntry{Name: plugin.Name, Source: plugin.Source}

		if err := validMarketplaceEntry(entry); err != nil {
			return "", nil, &RenderError{Component: marketplaceFile, Cause: err}
		}

		entries = append(entries, entry)
	}

	slices.SortFunc(entries, func(a, b MarketplaceEntry) int { return cmp.Compare(a.Name, b.Name) })

	return doc.Name, entries, nil
}

// validMarketplaceEntry reports why one entry cannot be listed.
func validMarketplaceEntry(entry MarketplaceEntry) error {
	if err := validIDPart(entry.Name); err != nil {
		return fmt.Errorf("the plugin name %w", err)
	}

	rel, ok := strings.CutPrefix(entry.Source, "./")
	if !ok || rel == "" {
		return fmt.Errorf("plugin %q: the source %q is not a dir below the marketplace root", entry.Name, entry.Source)
	}

	// A clean relative path that never climbs stays inside the root.
	if strings.ContainsRune(rel, 0) || path.IsAbs(rel) || path.Clean(rel) != strings.TrimSuffix(rel, "/") || climbs(rel) {
		return fmt.Errorf("plugin %q: the source %q leaves the marketplace root", entry.Name, entry.Source)
	}

	return nil
}
