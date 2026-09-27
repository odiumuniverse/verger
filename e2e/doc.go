// Package e2e is the container end-to-end driver for the Claude/Codex/Gemini
// host adapters (T1.13). The driver itself lives in driver_test.go behind the
// `e2e` build tag; this untagged file only keeps the directory buildable for
// repo-wide `go build ./...`/`go test ./...` runs.
package e2e
