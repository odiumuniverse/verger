package spec

import (
	"os"
	"path/filepath"
	"testing"
)

func mustParse(t *testing.T, data []byte) *Spec {
	t.Helper()

	spec, err := Parse(data)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	return spec
}

func mustParseFile(t *testing.T, path string) *Spec {
	t.Helper()

	spec, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse spec file %s: %v", path, err)
	}

	return spec
}

func mustMarshal(t *testing.T, spec *Spec) []byte {
	t.Helper()

	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	return data
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create parent of %s: %v", path, err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own fixture
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}

func asTable(t *testing.T, value any) map[string]any {
	t.Helper()

	table, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected a table, got %T", value)
	}

	return table
}

func asList(t *testing.T, value any) []any {
	t.Helper()

	list, ok := value.([]any)
	if !ok {
		t.Fatalf("expected an array, got %T", value)
	}

	return list
}
