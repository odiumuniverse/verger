package host

import "slices"

// Known reports whether id is one of the declared host ids.
func Known(id ID) bool {
	return slices.Contains(All(), id)
}

// All returns every host id; adapter availability is a separate check.
func All() []ID {
	return []ID{Claude, Codex, Gemini, Agy, Cursor, OpenCode, Kilo, Pi, DSH, Omp}
}
