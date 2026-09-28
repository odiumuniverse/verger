// Package e2e is the end-to-end driver for all ten host adapters (W3-E2E10):
// a fixture leg, a canon-package leg and a negative leg per host, run against
// the real CLIs on macOS and Linux. The driver itself lives in driver_test.go
// behind the `e2e` build tag; this untagged file only keeps the directory
// buildable for repo-wide `go build ./...`/`go test ./...` runs.
package e2e
