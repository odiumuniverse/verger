package secret

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// ExtractJSON moves literal secrets of a JSON document into the store and
// replaces them with {secret:NAME} refs, returning the rewritten document.
// Strings hidden behind keys ending in secret-ish names are checked for bare
// values; known {env:NAME} refs are canonicalized.
func ExtractJSON(data []byte, store *Store) ([]byte, bool, error) {
	var tree any

	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, false, fmt.Errorf("decode document: %w", err)
	}

	replaced := 0

	out := walkStrings(tree, "", func(key, value string) string {
		if name, ok := ParseEnvRef(value); ok {
			if store.Has(name) {
				replaced++

				return Ref(name)
			}

			return value
		}

		if !isSecret(key, value) {
			return value
		}

		name := store.nameFor(key, value)
		store.Set(name, value)

		replaced++

		return Ref(name)
	})

	if replaced == 0 {
		return data, false, nil
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false, fmt.Errorf("encode document: %w", err)
	}

	return encoded, true, nil
}

// ResolveJSON rewrites whole-string {secret:NAME} refs of a JSON document. In
// ModeEnv each ref becomes {env:NAME}; otherwise the stored value is written
// and unknown names render [redacted] and are returned sorted.
func ResolveJSON(data []byte, store *Store, mode string) ([]byte, []string, error) {
	var tree any

	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, nil, fmt.Errorf("decode document: %w", err)
	}

	missing := map[string]struct{}{}
	refs := 0

	out := walkStrings(tree, "", func(_, value string) string {
		name, ok := ParseRef(value)
		if !ok {
			return value
		}

		refs++

		if mode == ModeEnv {
			return EnvRef(name)
		}

		stored, ok := store.Get(name)
		if !ok {
			missing[name] = struct{}{}

			return redacted
		}

		return stored
	})

	if refs == 0 {
		return data, nil, nil
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, nil, fmt.Errorf("encode document: %w", err)
	}

	return encoded, slices.Sorted(maps.Keys(missing)), nil
}

// RefsJSON returns the sorted secret names referenced by a JSON document
// without touching it.
func RefsJSON(data []byte) ([]string, error) {
	var tree any

	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("decode document: %w", err)
	}

	names := map[string]struct{}{}

	walkStrings(tree, "", func(_, value string) string {
		if name, ok := ParseRef(value); ok {
			names[name] = struct{}{}
		}

		return value
	})

	return slices.Sorted(maps.Keys(names)), nil
}

// walkStrings rewrites every string in a decoded JSON tree with fn; container
// keys are passed to their scalars and array items inherit the array key.
func walkStrings(node any, key string, fn func(key, value string) string) any {
	switch typed := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))

		for _, child := range slices.Sorted(maps.Keys(typed)) {
			out[child] = walkStrings(typed[child], child, fn)
		}

		return out
	case []any:
		out := make([]any, len(typed))

		for i, item := range typed {
			out[i] = walkStrings(item, key, fn)
		}

		return out
	case string:
		return fn(key, typed)
	default:
		return node
	}
}
