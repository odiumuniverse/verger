package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/runtime"
)

// runtimePluginSpec describes the host surface that loads a rendered runtime
// module. A host with no declarative hook document still runs hooks: it loads
// a plugin, and the plugin is the shim the runtime renders. This is that
// placement — a directory the host resolves, plus the config entry that points
// at it.
//
// One host, one dialect. The shape is not a convention the other hosts can
// borrow, which is why it is per-host data rather than a flag.
type runtimePluginSpec struct {
	// host is the id recorded in the shim and in the runtime directory name.
	host string
	// dialect is the plugin dialect the host speaks: opencode v1 and v2,
	// kilo v1, pi.
	dialect string
	// surface locates the directory the host resolves local modules from.
	surface func(userHome, id string) string
	// layout places one rendered shim there. A host either reads a directory
	// the config document names, or scans a directory for modules; the two
	// need different files and different removals, and neither is derivable
	// from the other.
	layout pluginLayout
	// configFile resolves the document the entry is written to, for the
	// directory layout only. Which file that is differs per host (an existing
	// .jsonc wins over the default .json), so it stays host data.
	configFile func(surface string) string
}

// pluginLayout is how one host takes a rendered module.
type pluginLayout int

const (
	// layoutDirectory: a package directory with the shim and a package.json,
	// named by an entry in the host's config document (OpenCode v2).
	layoutDirectory pluginLayout = iota
	// layoutModule: one module file dropped into a directory the host scans
	// (Kilo v1, pi — both load every module in the directory).
	layoutModule
)

// pluginEntryKey is the config key the host reads its plugin list from.
const pluginEntryKey = "plugins"

// pluginFileName is the module file one package's shim is written as, for a
// host that scans a directory rather than reading a config entry.
func pluginFileName(id string) string {
	return "verger-" + escapePluginID(id) + ".ts"
}

// escapePluginID turns a package id into one safe path element.
func escapePluginID(id string) string {
	return strings.NewReplacer("/", "-", string(filepath.Separator), "-").Replace(id)
}

// runtimePlugin renders and places the module a host loads for one package:
// the bundle into the store, the manifest beside the package data, the shim
// into a plugin directory, and the config entry that makes the host resolve it.
//
// Every step is planned as an ordinary delivery step, so a dry run sees it, a
// removal reverses it, and a hand-edited file is hands-off like any other.
func (p *loosePlanner) runtimePlugin(ctx context.Context) error {
	// The gates first, and in this order: a host that declares its hooks
	// blocked, then the policy, then the delivery's own consent. A shim is the
	// hook as far as the host is concerned — a module in its plugin directory
	// is a hook it will run — so a blocked host must not get one, and saying
	// `delivered` for hooks nothing stands behind is worse than skipping them.
	proceed, err := p.runtimePluginGated()
	if err != nil || !proceed {
		return err
	}

	return p.placeRuntimePlugin(ctx)
}

// runtimePluginGated answers whether this delivery may place a shim at all.
func (p *loosePlanner) runtimePluginGated() (bool, error) {
	if p.spec.runtimePlugin == nil || len(p.pkg.Hooks) == 0 || !p.d.AllowHooks {
		return false, nil
	}

	// A host that declares its hooks blocked delivers no shim either: a module
	// in its plugin directory is a hook it runs. The reason is already
	// announced — `hooks()` is the one place that reports a blocked hook, and
	// this is the same block seen from the module side — so saying it a second
	// time would print the same line twice in every delivery report.
	if p.spec.hooksBlocked != "" {
		return false, nil
	}

	// The policy gate is the same one the declarative path uses: hooks it
	// forbids are never rendered, so their commands are neither rewritten nor
	// refused.
	permitted, err := p.hooksPermitted()
	if err != nil {
		return false, err
	}

	if !permitted {
		p.note("hooks skipped: allowManagedHooksOnly policy forbids plugin hooks")

		return false, nil
	}

	return true, nil
}

// placeRuntimePlugin renders the shim and puts it where the host resolves it.
func (p *loosePlanner) placeRuntimePlugin(ctx context.Context) error {
	spec := p.spec.runtimePlugin
	userHome := p.base.effectiveHome("")
	configDir := spec.surface(userHome, p.pkg.ID)
	dir := filepath.Join(configDir, "verger-"+escapePluginID(p.pkg.ID))

	runtimeDir, err := runtime.Install(ctx, p.base.store, spec.host, runtime.BundleVersion)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	table, err := runtime.LoadTable()
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	// The manifest lives with the package's data, not in the plugin directory:
	// the plugin directory is replaced on every shim rewrite, and the manifest
	// is the record that must survive it.
	dataDir := p.dataDir
	if dataDir == "" {
		return p.deliveryError(stepPlan, fmt.Errorf("%s: the package has no data directory for its runtime manifest", p.pkg.ID))
	}

	m := runtime.NewManifest(p.pkg.ID, p.pkg.Version, p.pkg.Hooks, p.hookScriptDigests())

	manifestPath, manifestSum, err := runtime.WriteManifest(dataDir, m)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	shim, err := runtime.RenderShim(runtime.ShimOptions{
		Host:           spec.host,
		Dialect:        spec.dialect,
		Package:        p.pkg.ID,
		Version:        p.pkg.Version,
		RuntimeDir:     runtimeDir,
		ManifestPath:   manifestPath,
		ManifestSHA256: manifestSum.String(),
		Consent:        consent.HookHash(p.pkg.ID, p.pkg.Version, p.pkg.Hooks).String(),
		LogPath:        filepath.Join(dataDir, "verger-runtime.log"),
	}, table)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	if spec.layout == layoutModule {
		// One module file in a directory the host scans: there is no entry
		// to write and no second file to place.
		return p.writeRuntimePluginFile(filepath.Join(spec.surface(userHome, p.pkg.ID), pluginFileName(p.pkg.ID)), shim)
	}

	if err := p.writeRuntimePluginFile(filepath.Join(dir, "index.js"), shim); err != nil {
		return err
	}

	doc, err := json.Marshal(map[string]string{
		"name":    escapePluginID(p.pkg.ID),
		"version": p.pkg.Version,
		"type":    "module",
		"main":    "index.js",
	})
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	if err := p.writeRuntimePluginFile(filepath.Join(dir, "package.json"), doc); err != nil {
		return err
	}

	if spec.layout == layoutModule {
		return nil
	}

	// The host's plugin list belongs to the host: it may already carry
	// packages verger did not place, and a rewritten array would drop them.
	// The existing entries are read and carried over, and a re-delivery
	// replaces only this package's own entry.
	configFile := spec.configFile(configDir)

	existing, err := readOptionalFile(configFile)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	merged, err := mergePluginEntry(existing, pluginEntryKey, dir)
	if err != nil {
		return p.deliveryError(stepPlan, err)
	}

	// The plugin directory is verger's own and the removal deletes both of its
	// files, so the directory has to go with them. It cannot be a tree op: the
	// digest of a tree is taken over what is on disk, and at plan time nothing
	// is written yet. The removal therefore prunes the directory it emptied.
	p.prunedDirs = append(p.prunedDirs, dir)

	p.queueConfigEdit(configFile, false, render.EditJSONC, pendingEdit{
		edit: render.Edit{Path: pluginEntryKey, Value: merged},
		kind: "runtime",
		name: p.pkg.ID,
		// The list belongs to the host: the user adds and removes their own
		// plugins in the same array, so owning it whole would make every
		// delivery hands-off the moment a foreign entry exists. Verger owns
		// its own entry and reads the rest (DRIFT-2).
		markPerRecord: true,
	})

	return nil
}

// mergePluginEntry returns the host's plugin list with dir in it: the entries
// already declared, minus this package's own, plus the new one. An entry the
// user removed by hand comes back — the delivery is the claim that this
// package is here — and an entry for a different package is never touched.
func mergePluginEntry(document []byte, key, dir string) ([]any, error) {
	var out []any

	if len(bytes.TrimSpace(document)) > 0 {
		parsed, err := jsoncToJSON(document)
		if err != nil {
			return nil, err
		}

		var decoded map[string]any
		if err := json.Unmarshal(parsed, &decoded); err != nil {
			return nil, err
		}

		if raw, ok := decoded[key].([]any); ok {
			out = raw
		}
	}

	kept := make([]any, 0, len(out)+1)

	for _, entry := range out {
		if name, ok := entry.(string); ok && pluginDirOf(name) == dir {
			continue
		}

		kept = append(kept, entry)
	}

	return append(kept, dir), nil
}

// pluginDirOf resolves a plugin list entry to the directory it names. A host
// also accepts npm and git specifiers in this list, and those are not
// directories; they are carried through untouched.
func pluginDirOf(entry string) string {
	if strings.Contains(entry, "://") {
		return ""
	}

	return entry
}

// writeRuntimePluginFile plans one file of the plugin directory. The files are
// verger's own, so they are written whole: a plugin directory with a hand-edited
// shim is a module the runtime no longer vouches for, and the receipt names the
// files so a removal can take them.
func (p *loosePlanner) writeRuntimePluginFile(path string, data []byte) error {
	sum := digest.Bytes(data)
	_, existed := p.ownerOf(path)

	p.plan.add(
		looseStep{kind: stepWriteFile, path: path, data: data, mode: 0o600, existed: existed, digest: sum},
		receipt.Artifact{Kind: "runtime", Name: filepath.Base(path), Path: path, Digest: sum},
		receipt.Op{Kind: receipt.OpWriteFile, Path: path, Digest: sum, Mode: 0o600, Existed: existed},
	)

	return nil
}

// hookScriptDigests is the script digest map the manifest carries. A command
// hook runs an arbitrary command line, not a file verger delivered, so there
// is nothing verger can vouch for here: the manifest records the hooks and the
// consent hash is what the runtime checks.
func (p *loosePlanner) hookScriptDigests() map[string]digest.Hash {
	return nil
}
