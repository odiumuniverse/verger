package home

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	. "github.com/smartystreets/goconvey/convey"
)

// leasePath returns a temp state path for lease tests.
func leasePath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "state", "watch.lease")
}

// readLeaseFile parses the on-disk lease.
func readLeaseFile(t *testing.T, path string) Lease {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads a path it created
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}

	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		t.Fatalf("parse lease: %v", err)
	}

	return lease
}

// assertLease compares two leases field by field (times via Equal).
func assertLease(t *testing.T, got, want Lease) {
	t.Helper()

	if got.PID != want.PID || got.Owner != want.Owner || !got.Since.Equal(want.Since) {
		t.Fatalf("lease = %+v, want %+v", got, want)
	}
}

func TestLeaseAcquireRelease(t *testing.T) {
	Convey("Given a lease path", t, func() {
		path := leasePath(t)

		Convey("When the lease is acquired", func() {
			handle, err := AcquireLease(path, LeaseOwnerVerger)

			Convey("Then the file carries owner, pid and a UTC timestamp", func() {
				So(err, ShouldBeNil)
				assertMode(t, filepath.Dir(path), 0o700)
				assertMode(t, path, 0o600)

				info := handle.Info()
				So(info.Owner, ShouldEqual, LeaseOwnerVerger)
				So(info.PID, ShouldEqual, os.Getpid())
				So(info.Since.IsZero(), ShouldBeFalse)
				So(info.Since.Location(), ShouldEqual, time.UTC)
				So(time.Since(info.Since) > -time.Minute, ShouldBeTrue)

				onDisk := readLeaseFile(t, path)
				assertLease(t, onDisk, info)
			})

			Convey("Then LeaseStatus reports it held by this owner", func() {
				So(err, ShouldBeNil)

				info, held, statusErr := LeaseStatus(path)
				So(statusErr, ShouldBeNil)
				So(held, ShouldBeTrue)
				assertLease(t, info, handle.Info())
			})

			Convey("Then Release is idempotent and leaves the file in place", func() {
				So(err, ShouldBeNil)
				So(handle.Release(), ShouldBeNil)
				So(handle.Release(), ShouldBeNil)

				_, statErr := os.Stat(path)
				So(statErr, ShouldBeNil)

				info, held, statusErr := LeaseStatus(path)
				So(statusErr, ShouldBeNil)
				So(held, ShouldBeFalse)
				assertLease(t, info, handle.Info())
			})

			Convey("Then a second acquire reports LeaseHeldError with the owner", func() {
				So(err, ShouldBeNil)

				_, secondErr := AcquireLease(path, LeaseOwnerBeadle)

				target, ok := errors.AsType[*LeaseHeldError](secondErr)
				So(ok, ShouldBeTrue)
				So(target.Owner, ShouldEqual, LeaseOwnerVerger)
				So(target.PID, ShouldEqual, os.Getpid())
				So(target.Since.IsZero(), ShouldBeFalse)
				So(secondErr.Error(), ShouldContainSubstring, LeaseOwnerVerger)

				So(handle.Release(), ShouldBeNil)
			})
		})
	})
}

func TestLeaseStatusStates(t *testing.T) {
	Convey("Given a lease path", t, func() {
		path := leasePath(t)

		Convey("When the file is missing", func() {
			info, held, err := LeaseStatus(path)

			Convey("Then it is not held", func() {
				So(err, ShouldBeNil)
				So(held, ShouldBeFalse)
				assertLease(t, info, Lease{})
			})
		})

		Convey("When a stale lease file exists without a holder", func() {
			stale := Lease{
				PID:   999999,
				Owner: LeaseOwnerBeadle,
				Since: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
			}

			data, marshalErr := json.Marshal(stale)
			So(marshalErr, ShouldBeNil)
			mkFile(t, path, string(data), 0o600)

			info, held, err := LeaseStatus(path)

			Convey("Then the content is reported but the lease is not held", func() {
				So(err, ShouldBeNil)
				So(held, ShouldBeFalse)
				assertLease(t, info, stale)
			})
		})

		Convey("When the lease file is malformed and unlocked", func() {
			mkFile(t, path, "{not json", 0o600)

			info, held, err := LeaseStatus(path)

			Convey("Then it is not held with zero info and no error", func() {
				So(err, ShouldBeNil)
				So(held, ShouldBeFalse)
				assertLease(t, info, Lease{})
			})
		})

		Convey("When a held lease file is empty", func() {
			handle, err := AcquireLease(path, LeaseOwnerVerger)
			So(err, ShouldBeNil)
			So(os.Truncate(path, 0), ShouldBeNil)

			info, held, statusErr := LeaseStatus(path)

			Convey("Then it is held with zero info and no error", func() {
				So(statusErr, ShouldBeNil)
				So(held, ShouldBeTrue)
				assertLease(t, info, Lease{})
			})

			Convey("Then Release cleans up", func() {
				So(handle.Release(), ShouldBeNil)
			})
		})

		Convey("When a stale lease names a dead pid", func() {
			stale := Lease{PID: 1 << 30, Owner: LeaseOwnerBeadle, Since: time.Now().UTC()}

			data, marshalErr := json.Marshal(stale)
			So(marshalErr, ShouldBeNil)
			mkFile(t, path, string(data), 0o600)

			info, held, err := LeaseStatus(path)
			So(err, ShouldBeNil)
			So(held, ShouldBeFalse)
			assertLease(t, info, stale)

			Convey("Then the lease can be stolen and its content replaced", func() {
				handle, acquireErr := AcquireLease(path, LeaseOwnerVerger)
				So(acquireErr, ShouldBeNil)

				now := handle.Info()
				So(now.Owner, ShouldEqual, LeaseOwnerVerger)
				So(now.PID, ShouldEqual, os.Getpid())

				assertLease(t, readLeaseFile(t, path), now)
				So(handle.Release(), ShouldBeNil)
			})
		})

		Convey("When acquiring over a malformed file", func() {
			mkFile(t, path, "garbage", 0o600)

			handle, err := AcquireLease(path, LeaseOwnerVerger)

			Convey("Then the content is replaced with valid JSON", func() {
				So(err, ShouldBeNil)
				assertLease(t, readLeaseFile(t, path), handle.Info())
				So(handle.Release(), ShouldBeNil)
			})
		})
	})
}

func TestLeaseHoldsRealFlock(t *testing.T) {
	Convey("Given a held lease", t, func() {
		path := leasePath(t)

		handle, err := AcquireLease(path, LeaseOwnerVerger)
		So(err, ShouldBeNil)

		Convey("When an external gofrs/flock handle probes the lease file", func() {
			probe := flock.New(path)

			locked, probeErr := probe.TryLock()
			So(probeErr, ShouldBeNil)

			Convey("Then it sees the lease held and can take it after release", func() {
				So(locked, ShouldBeFalse)

				So(handle.Release(), ShouldBeNil)

				free, freeErr := probe.TryLock()
				So(freeErr, ShouldBeNil)
				So(free, ShouldBeTrue)
				So(probe.Unlock(), ShouldBeNil)
			})
		})
	})
}

func TestLeaseRefresh(t *testing.T) {
	Convey("Given a lease path", t, func() {
		path := leasePath(t)

		first, err := AcquireLease(path, LeaseOwnerVerger)
		So(err, ShouldBeNil)

		firstInfo := first.Info()
		So(first.Release(), ShouldBeNil)

		time.Sleep(2 * time.Millisecond)

		Convey("When the lease is re-acquired after release", func() {
			second, acquireErr := AcquireLease(path, LeaseOwnerVerger)

			Convey("Then it is a refresh with a new timestamp", func() {
				So(acquireErr, ShouldBeNil)

				secondInfo := second.Info()
				So(secondInfo.Since.Equal(firstInfo.Since), ShouldBeFalse)
				So(secondInfo.Since.After(firstInfo.Since), ShouldBeTrue)
				So(second.Release(), ShouldBeNil)
			})
		})
	})
}

func TestLeaseConcurrentAcquire(t *testing.T) {
	Convey("Given a lease path", t, func() {
		path := leasePath(t)

		Convey("When ten goroutines acquire concurrently", func() {
			const n = 10

			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				winners   []*LeaseHandle
				conflicts int
				other     []error
			)

			for range n {
				wg.Go(func() {
					handle, err := AcquireLease(path, LeaseOwnerVerger)

					mu.Lock()
					defer mu.Unlock()

					switch {
					case err == nil:
						winners = append(winners, handle)
					case isLeaseHeld(err):
						conflicts++
					default:
						other = append(other, err)
					}
				})
			}

			wg.Wait()

			Convey("Then exactly one wins and the rest see LeaseHeldError", func() {
				So(other, ShouldBeEmpty)
				So(winners, ShouldHaveLength, 1)
				So(conflicts, ShouldEqual, n-1)
				So(winners[0].Release(), ShouldBeNil)
			})
		})
	})
}

// isLeaseHeld reports whether err is a *LeaseHeldError.
func isLeaseHeld(err error) bool {
	_, ok := errors.AsType[*LeaseHeldError](err)

	return ok
}

func TestLeaseStatusErrors(t *testing.T) {
	Convey("Given a lease path", t, func() {
		path := leasePath(t)

		Convey("When the lease file is unreadable", func() {
			mkFile(t, path, `{"pid":1,"owner":"verger"}`, 0o000)

			_, _, err := LeaseStatus(path)

			Convey("Then the IO error surfaces", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestLeaseAcquirePathErrors(t *testing.T) {
	Convey("Given broken lease paths", t, func() {
		Convey("When the lease path is a directory", func() {
			path := filepath.Join(t.TempDir(), "state", "watch.lease")
			So(os.MkdirAll(path, 0o700), ShouldBeNil)

			_, err := AcquireLease(path, LeaseOwnerVerger)

			Convey("Then acquisition fails", func() {
				So(err, ShouldBeError)
			})
		})

		Convey("When the lease parent is a file", func() {
			blocker := filepath.Join(t.TempDir(), "blocker")
			mkFile(t, blocker, "not a dir", 0o600)

			_, err := AcquireLease(filepath.Join(blocker, "watch.lease"), LeaseOwnerVerger)

			Convey("Then acquisition fails", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestLeaseInvalidOwner(t *testing.T) {
	Convey("Given a lease path", t, func() {
		path := leasePath(t)

		Convey("When the owner is empty or whitespace", func() {
			for _, owner := range []string{"", "   "} {
				_, err := AcquireLease(path, owner)

				Convey("Then owner "+fmt.Sprintf("%q", owner)+" is rejected as InvalidOwnerError", func() {
					target, ok := errors.AsType[*InvalidOwnerError](err)
					So(ok, ShouldBeTrue)
					So(target.Value, ShouldEqual, owner)
					So(err.Error(), ShouldContainSubstring, "owner")
				})
			}
		})
	})
}

// TestAcquireTakesOverAStaleLease pins the reclaim. A lease file outlives the
// process that wrote it whenever the machine is killed rather than shut
// down, and a watcher that refuses forever because a dead pid is in a file
// is a watcher that never comes back after a crash — the same reason the
// lock is taken before the file is read.
func TestAcquireTakesOverAStaleLease(t *testing.T) {
	Convey("Given a lease file left by a process that is gone", t, func() {
		path := leasePath(t)

		stale := Lease{
			PID:   deadPID(t),
			Owner: LeaseOwnerBeadle,
			Since: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		}

		data, err := json.Marshal(stale)
		So(err, ShouldBeNil)
		mkFile(t, path, string(data), 0o600)

		Convey("Then acquiring succeeds and names the new holder", func() {
			handle, err := AcquireLease(path, LeaseOwnerVerger)
			So(err, ShouldBeNil)
			So(handle, ShouldNotBeNil)

			So(handle.Info().Owner, ShouldEqual, LeaseOwnerVerger)
			So(handle.Info().PID, ShouldEqual, os.Getpid())

			Convey("And the file on disk is the new lease, not the stale one", func() {
				info, _, statusErr := LeaseStatus(path)
				So(statusErr, ShouldBeNil)
				So(info.Owner, ShouldEqual, LeaseOwnerVerger)
				So(info.PID, ShouldEqual, os.Getpid())
			})

			So(handle.Release(), ShouldBeNil)
		})
	})
}

// TestAcquireRefusesALiveForeignLease is the other half: a lease whose holder
// is alive is not stale, and taking it would put two watchers on one home.
func TestAcquireRefusesALiveForeignLease(t *testing.T) {
	Convey("Given a lease held right now by another owner", t, func() {
		path := leasePath(t)

		handle, err := AcquireLease(path, LeaseOwnerBeadle)
		So(err, ShouldBeNil)

		Convey("Then a second acquire is refused and names the holder", func() {
			_, err := AcquireLease(path, LeaseOwnerVerger)
			So(err, ShouldNotBeNil)

			held, ok := errors.AsType[*LeaseHeldError](err)
			So(ok, ShouldBeTrue)
			So(held.Owner, ShouldEqual, LeaseOwnerBeadle)
			So(err.Error(), ShouldContainSubstring, LeaseOwnerBeadle)
		})

		So(handle.Release(), ShouldBeNil)
	})
}

// deadPID returns a pid that is not running. It starts a short-lived child
// and waits for it, so the number is one the OS has actually reaped rather
// than one this test made up and hopes is unused.
func deadPID(t *testing.T) int {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "true")
	So(cmd.Run(), ShouldBeNil)

	return cmd.Process.Pid
}

func TestLeaseStatusReadsBeforeReleasingProbe(t *testing.T) {
	Convey("Given a stale lease file", t, func() {
		path := leasePath(t)

		stale := Lease{
			PID:   4242,
			Owner: LeaseOwnerBeadle,
			Since: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		}

		data, err := json.Marshal(stale)
		So(err, ShouldBeNil)
		mkFile(t, path, string(data), 0o600)

		probeLocked := make(chan bool, 1)

		read := func(readPath string) (Lease, error) {
			probe := flock.New(readPath)

			locked, probeErr := probe.TryLock()
			if probeErr == nil && locked {
				_ = probe.Unlock()
			}

			probeLocked <- locked

			return loadLeaseFile(readPath)
		}

		Convey("When LeaseStatus probes it", func() {
			info, held, statusErr := leaseStatus(path, read)

			Convey("Then the probe lock is still held while the content is read", func() {
				So(statusErr, ShouldBeNil)
				So(held, ShouldBeFalse)
				assertLease(t, info, stale)
				So(<-probeLocked, ShouldBeFalse)
			})
		})
	})
}

func TestLeaseStatusReadOnlyFile(t *testing.T) {
	Convey("Given a read-only stale lease file", t, func() {
		path := leasePath(t)

		stale := Lease{
			PID:   777,
			Owner: LeaseOwnerBeadle,
			Since: time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC),
		}

		data, err := json.Marshal(stale)
		So(err, ShouldBeNil)
		mkFile(t, path, string(data), 0o600)
		So(os.Chmod(path, 0o400), ShouldBeNil)

		Convey("When LeaseStatus probes it", func() {
			info, held, statusErr := LeaseStatus(path)

			Convey("Then it reports the record without needing write access", func() {
				So(statusErr, ShouldBeNil)
				So(held, ShouldBeFalse)
				assertLease(t, info, stale)
			})
		})
	})
}
