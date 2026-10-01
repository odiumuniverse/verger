package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Heartbeat is what the runtime writes into its directory when it loads: the
// proof that a host actually executed it. The Go side reads it to tell a
// healthy runtime from one a host never managed to load (DESIGN §4.6: a broken
// runtime rolls back to the previous one by heartbeat).
type Heartbeat struct {
	At      time.Time `json:"at"`
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	Dialect string    `json:"dialect"`
	Package string    `json:"package"`
}

// HeartbeatPath is the heartbeat document of one runtime directory.
func HeartbeatPath(dir string) string {
	return filepath.Join(dir, heartbeatDoc)
}

// ReadHeartbeat reads one runtime directory's heartbeat; ok is false when the
// runtime has never run.
func ReadHeartbeat(dir string) (Heartbeat, bool, error) {
	data, err := readOptional(HeartbeatPath(dir))
	if err != nil {
		return Heartbeat{}, false, err
	}

	if len(data) == 0 {
		return Heartbeat{}, false, nil
	}

	var hb Heartbeat

	if err := json.Unmarshal(data, &hb); err != nil {
		return Heartbeat{}, false, fmt.Errorf("parse runtime heartbeat %s: %w", HeartbeatPath(dir), err)
	}

	return hb, true, nil
}

// Healthy reports whether the runtime in dir has run within the window: the
// signal the adapter uses before pointing shims at a freshly installed
// runtime, and the one that sends them back to the previous version.
func Healthy(dir string, now time.Time, within time.Duration) bool {
	hb, ok, err := ReadHeartbeat(dir)
	if err != nil || !ok {
		return false
	}

	return now.Sub(hb.At) <= within
}

// PreviousVersion returns the newest runtime version of one host other than
// the given one, preferring a healthy one: the directory a rollback points the
// shims back at. ok is false when there is nothing to roll back to.
func PreviousVersion(st *store.Store, host, current string, now time.Time, within time.Duration) (string, bool) {
	root := filepath.Join(st.Root(), "runtime", host)

	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}

	type candidate struct {
		version string
		at      time.Time
		healthy bool
	}

	var candidates []candidate

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == current {
			continue
		}

		dir := filepath.Join(root, entry.Name())

		hb, ok, err := ReadHeartbeat(dir)
		if err != nil || !ok {
			continue
		}

		candidates = append(candidates, candidate{version: entry.Name(), at: hb.At, healthy: now.Sub(hb.At) <= within})
	}

	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.healthy != b.healthy {
			if a.healthy {
				return -1
			}

			return 1
		}

		return b.at.Compare(a.at)
	})

	if len(candidates) == 0 {
		return "", false
	}

	return candidates[0].version, true
}

// WriteHeartbeat records one heartbeat. The Go side uses it in tests and in the
// delivery that proves a shim loaded; the bundle writes the same document when
// a host executes it.
func WriteHeartbeat(dir string, hb Heartbeat) error {
	if err := fsutil.EnsureDir(dir, 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime heartbeat: %w", err)
	}

	return fsutil.WriteFileAtomicCAS(HeartbeatPath(dir), append(data, '\n'), 0o600)
}
