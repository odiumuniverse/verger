package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/tailscale/hujson"

	toml "github.com/pelletier/go-toml/v2"
)

// Host policy documents (DESIGN §1.3/§4.8): a marketplace a host policy
// forbids is never registered by any strategy. This is the single framework
// policy checker every adapter routes through.
const (
	policyBlocked = "blockedMarketplaces"
	policyStrict  = "strictKnownMarketplaces"
	policyCLI     = "disableCommandPluginSources"
	policyHooks   = "allowManagedHooksOnly"
)

// checkPolicy refuses a source ref forbidden by a host policy document. ref is
// the delivery's source ref (Package.Marketplace) for every stratum. An empty
// ref cannot satisfy an allowlist, so a non-empty strictKnownMarketplaces
// blocks it (fail closed), while a blocklist has nothing to match. An
// unreadable, unparsable or malformed policy document fails closed.
func checkPolicy(host ID, home, ref string) error {
	for _, file := range policyFiles(host, home) {
		if err := checkPolicyFile(host, file, ref); err != nil {
			return err
		}
	}

	return nil
}

// policyFiles lists the user and managed policy documents of a host.
func policyFiles(host ID, home string) []string {
	switch host {
	case Claude:
		files := []string{filepath.Join(claudeConfigDir(home), "settings.json")}

		switch runtime.GOOS {
		case "darwin":
			files = append(files, "/Library/Application Support/ClaudeCode/managed-settings.json")
		case "linux":
			files = append(files, "/etc/claude-code/managed-settings.json")
		}

		return files
	case Codex:
		files := []string{filepath.Join(codexConfigDir(home), "config.toml")}

		switch runtime.GOOS {
		case "darwin":
			files = append(files, "/Library/Application Support/Codex/managed.toml")
		case "linux":
			files = append(files, "/etc/codex/managed.toml")
		}

		return files
	case Gemini:
		files := []string{filepath.Join(geminiConfigDir(home), "settings.json")}

		switch runtime.GOOS {
		case "darwin":
			files = append(files, "/Library/Application Support/GeminiCli/settings.json")
		case "linux":
			files = append(files, "/etc/gemini-cli/settings.json")
		}

		return files
	case Omp:
		// Conscious nil: omp has no managed policy document. Its settings file
		// (`<agentDir>/config.yml`) carries UI and approval preferences and no
		// marketplace allow/deny list, and there is no `EditYAML` to write it
		// anyway — a policy verger cannot read is not a policy it may claim.
		return nil
	case OpenCode, Kilo:
		// Conscious nil: neither host ships a managed policy document
		// (OpenCode's config carries plugins, MCP servers and permissions, and
		// the binary knows none of the Claude managed keys — live-checked on
		// 2.0.18). The shared dispatch still routes both hosts through
		// checkPolicy, so the gate is in place the day such a document exists;
		// today nothing is read and nothing is claimed.
		return nil
	default:
		return nil
	}
}

// checkCommandSources refuses a native/synth CLI install when the host policy
// disables command plugin sources (DESIGN §1.3).
func checkCommandSources(host ID, home, ref string) error {
	disabled, err := policyBool(host, home, policyCLI)
	if err != nil {
		return err
	}

	if disabled {
		return &PolicyError{Host: host, Rule: policyCLI, Ref: ref}
	}

	return nil
}

// hooksAllowed reports whether the host policy lets verger deliver plugin
// hooks; `allowManagedHooksOnly: true` forbids them.
func hooksAllowed(host ID, home string) (bool, error) {
	managedOnly, err := policyBool(host, home, policyHooks)
	if err != nil {
		return false, err
	}

	return !managedOnly, nil
}

// policyBool reports whether any policy document of the host sets key to true;
// an unreadable or unparsable document fails closed.
func policyBool(host ID, home, key string) (bool, error) {
	for _, file := range policyFiles(host, home) {
		data, err := os.ReadFile(file) //nolint:gosec // G304: the path is a host settings file or the platform managed path
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return false, &DeliveryError{Host: string(host), Step: stepPolicy, Cause: fmt.Errorf("read %s: %w", file, err)}
		}

		doc, err := policyDocument(file, data)
		if err != nil {
			return false, &DeliveryError{Host: string(host), Step: stepPolicy, Cause: fmt.Errorf("parse %s: %w", file, err)}
		}

		if value, ok := doc[key].(bool); ok && value {
			return true, nil
		}
	}

	return false, nil
}

// checkPolicyFile applies one policy document; JSONC and TOML are both read.
func checkPolicyFile(host ID, file, ref string) error {
	data, err := os.ReadFile(file) //nolint:gosec // G304: the path is a host settings file or the platform managed path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return &DeliveryError{Host: string(host), Step: stepPolicy, Cause: fmt.Errorf("read %s: %w", file, err)}
	}

	doc, err := policyDocument(file, data)
	if err != nil {
		return &DeliveryError{Host: string(host), Step: stepPolicy, Cause: fmt.Errorf("parse %s: %w", file, err)}
	}

	blocked, _, err := policyList(doc, policyBlocked)
	if err != nil {
		return &DeliveryError{Host: string(host), Step: stepPolicy, Cause: fmt.Errorf("%s: %w", file, err)}
	}

	strict, allowlist, err := policyList(doc, policyStrict)
	if err != nil {
		return &DeliveryError{Host: string(host), Step: stepPolicy, Cause: fmt.Errorf("%s: %w", file, err)}
	}

	if ref != "" && matchesAnyRef(blocked, ref) {
		return &PolicyError{Host: host, Rule: policyBlocked, Ref: ref}
	}

	// A present allowlist gates by its presence: `[]` is complete lockdown
	// (Claude docs, NF-4).
	if allowlist && (ref == "" || !matchesAnyRef(strict, ref)) {
		return &PolicyError{Host: host, Rule: policyStrict, Ref: ref}
	}

	return nil
}

// policyList returns one list-valued policy key and whether it is present; a
// null value is absent, and a present key that is not a list cannot be
// applied, which is an error rather than no policy.
func policyList(doc map[string]any, key string) ([]any, bool, error) {
	value, ok := doc[key]
	if !ok || value == nil {
		return nil, false, nil
	}

	entries, ok := value.([]any)
	if !ok {
		return nil, false, fmt.Errorf("policy %s is a %T, not a list", key, value)
	}

	return entries, true, nil
}

// policyDocument decodes one policy document by its extension; an empty
// document carries no policies.
func policyDocument(file string, data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}

	if strings.HasSuffix(file, tomlExt) {
		doc := map[string]any{}

		if err := toml.Unmarshal(data, &doc); err != nil {
			return nil, err
		}

		return doc, nil
	}

	root, err := hujson.Parse(data)
	if err != nil {
		return nil, err
	}

	standard, err := hujson.Standardize(root.Pack())
	if err != nil {
		return nil, err
	}

	doc := map[string]any{}

	if err := json.Unmarshal(standard, &doc); err != nil {
		return nil, err
	}

	return doc, nil
}

// matchesAnyRef reports whether any policy entry matches the ref.
func matchesAnyRef(entries []any, ref string) bool {
	for _, entry := range entries {
		if matchesRef(entry, ref) {
			return true
		}
	}

	return false
}

// matchesRef matches a policy entry (string or source object) against the ref
// and its derived marketplace name.
func matchesRef(entry any, ref string) bool {
	candidates := []string{ref}

	if name := marketplaceName(ref); name != "" && name != ref {
		candidates = append(candidates, name)
	}

	switch typed := entry.(type) {
	case string:
		return slices.Contains(candidates, typed)
	case map[string]any:
		for _, key := range []string{"source", "repo", "url", "path", "name", "owner"} {
			if value, ok := typed[key].(string); ok && slices.Contains(candidates, value) {
				return true
			}
		}
	}

	return false
}

// marketplaceName derives the registered marketplace name of a ref: a URL or
// path keeps its last segment without .git, a plain name stays itself.
func marketplaceName(ref string) string {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(ref), "/"), ".git")

	if strings.Contains(trimmed, "://") {
		if parsed, err := url.Parse(trimmed); err == nil {
			return path.Base(strings.TrimSuffix(parsed.Path, "/"))
		}
	}

	if index := strings.LastIndexByte(trimmed, '/'); index >= 0 {
		return trimmed[index+1:]
	}

	return trimmed
}
