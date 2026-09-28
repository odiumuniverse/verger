package verger

import (
	"context"
	"errors"
	"strings"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/pack"
	"github.com/odiumuniverse/verger/pkg/source"
)

// buildHostPackage lifts one fetched payload into the host.Package the
// adapters take.
func (c *Client) buildHostPackage(fetched *source.Fetched, id host.ID, version, scope, projectRoot string) (host.Package, error) {
	meta := fetched.Package

	dataDir, err := c.Store().PackageDataPath(idString(meta), string(id))
	if err != nil {
		return host.Package{}, err
	}

	return host.Package{
		ID:          idString(meta),
		Version:     firstNonEmpty(version, meta.Version),
		Format:      meta.Format,
		Root:        fetched.Root,
		Components:  meta.Components,
		MCP:         meta.MCP,
		Hooks:       meta.Hooks,
		Scope:       scope,
		ProjectRoot: projectRoot,
		DataDir:     dataDir,
	}, nil
}

// idString renders the canonical id of one parsed manifest package.
func idString(meta *manifest.Package) string {
	if meta.ID != "" {
		return meta.ID
	}

	return meta.Name
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// PickStrategy is the Ф1 stopgap ladder (TODO T2.1, OQ-T1.12.1): native when
// the payload carries a marketplace ref and the host CLI resolves; synth for
// remote payloads pkg/pack can render; loose otherwise (local dev payloads are
// always loose, DESIGN §2.1). It is exported because a second front end that
// previews a plan needs the same ladder the executor uses.
func PickStrategy(pkg host.Package, hostID host.ID, kind source.Kind) (host.Strategy, string) {
	if pkg.Marketplace != "" {
		if nativeCLIResolves(hostID) {
			return host.Native, ""
		}

		return host.Loose, "native skipped: the package carries a marketplace ref, but the " + string(hostID) + " CLI does not resolve on PATH"
	}

	if synthRenderable(pkg, kind) {
		return host.Synth, "synth: pkg/pack rendered the payload for the host, so no host CLI is needed"
	}

	if kind == source.KindLocal || kind == "" {
		return host.Loose, "a local payload is always delivered loose (DESIGN §2.1)"
	}

	if owner, _ := SplitID(pkg.ID); owner == "" {
		return host.Loose, "synth skipped: the package id needs owner/name"
	}

	return host.Loose, "synth skipped: pkg/pack cannot render this payload for the host"
}

// synthRenderable reports whether pkg/pack can render the package for a
// remote-sourced payload with an owner.
func synthRenderable(pkg host.Package, kind source.Kind) bool {
	if kind == source.KindLocal || kind == "" {
		return false
	}

	if owner, _ := SplitID(pkg.ID); owner == "" {
		return false
	}

	input, err := packInput(pkg, pkg.Root)
	if err != nil {
		return false
	}

	_, err = pack.Render(input)

	return err == nil
}

// nativeCLIResolves reports whether the host's own CLI resolves on PATH.
func nativeCLIResolves(id host.ID) bool {
	name, ok := HostCLIName(id)
	if !ok {
		return false
	}

	_, err := hostcli.NewResolver().Resolve(name)

	return err == nil
}

// HostCLIName maps one host id to its CLI binary name.
func HostCLIName(id host.ID) (string, bool) {
	switch id {
	case host.Claude:
		return "claude", true
	case host.Codex:
		return "codex", true
	case host.Gemini:
		return "gemini", true
	case host.Agy:
		return "agy", true
	case host.Cursor:
		return "cursor", true
	case host.OpenCode:
		return "opencode", true
	case host.Kilo:
		return "kilo", true
	case host.Pi:
		return "pi", true
	case host.DSH:
		return "dsh", true
	case host.Omp:
		return "omp", true
	default:
		return "", false
	}
}

// packInput builds the chimera input of one host package.
func packInput(pkg host.Package, root string) (pack.Input, error) {
	owner, name := SplitID(pkg.ID)
	if owner == "" || name == "" {
		return pack.Input{}, errors.New("the package id needs owner/name")
	}

	return pack.Input{
		ID: pkg.ID, Name: name, Owner: owner, Version: pkg.Version,
		Description: "", License: "", Format: pkg.Format,
		Root: root, Components: pkg.Components, MCP: pkg.MCP, Hooks: pkg.Hooks,
	}, nil
}

// SplitID splits one owner/name id.
func SplitID(id string) (string, string) {
	if index := strings.LastIndexByte(id, '/'); index >= 0 {
		return id[:index], id[index+1:]
	}

	return "", id
}

// prepareDelivery fills the strategy-specific fields of one delivery.
func (c *Client) prepareDelivery(ctx context.Context, pkg host.Package, strategy host.Strategy) (host.Package, []string, error) {
	if strategy != host.Synth {
		return pkg, nil, nil
	}

	owner, name := SplitID(pkg.ID)
	if owner == "" {
		return pkg, nil, errors.New("synth needs an owner/name package id")
	}

	input, err := packInput(pkg, pkg.Root)
	if err != nil {
		return pkg, nil, err
	}

	result, err := pack.Write(ctx, c.Store(), input)
	if err != nil {
		return pkg, nil, err
	}

	pkg.SynthDir = result.Dir
	pkg.Marketplace = name + "@" + owner

	return pkg, []string{"synth: " + result.Dir}, nil
}

// Fetch resolves one ref through the source resolver, for a caller that reads a
// ref out of a document (approve, pack) rather than out of a command line. The
// fetcher is store-backed, so a fetched payload lands in this client's store.
func (c *Client) Fetch(ctx context.Context, ref source.Ref) (*source.Fetched, error) {
	fetcher, err := source.NewFetcher(source.WithStore(c.Store()))
	if err != nil {
		return nil, err
	}

	return fetcher.Fetch(ctx, ref)
}

// PrepareDelivery fills the strategy-specific fields of one delivery: a synth
// strategy renders the chimera package into the store and names it. It is
// exported for a front end that plans its own actions from a facade plan.
func (c *Client) PrepareDelivery(ctx context.Context, pkg host.Package, strategy host.Strategy) (host.Package, []string, error) {
	return c.prepareDelivery(ctx, pkg, strategy)
}
