package cli

import (
	"context"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// slowHost answers its oracle only after a fixed delay, and records the most
// concurrent probes it ever saw. Both numbers matter: the peak says the hosts
// overlapped, the wall clock says the overlap was real and not bookkeeping.
type slowHost struct {
	id   host.ID
	wait time.Duration

	mu      sync.Mutex
	running int
	peak    int
	calls   int
}

func newSlowHost(id host.ID, wait time.Duration) *slowHost {
	return &slowHost{id: id, wait: wait}
}

func (s *slowHost) ID() host.ID { return s.id }

func (s *slowHost) Detect(string) bool { return true }

func (s *slowHost) Oracle() host.Oracle { return slowOracle{host: s} }

func (s *slowHost) Deliver(context.Context, string, host.Delivery) (host.Result, error) {
	return host.Result{}, nil
}

func (s *slowHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	return host.Result{}, nil
}

func (s *slowHost) peakProbes() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.peak
}

func (s *slowHost) probeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

func (s *slowHost) enter() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.running++
	s.calls++

	if s.running > s.peak {
		s.peak = s.running
	}
}

func (s *slowHost) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.running--
}

type slowOracle struct{ host *slowHost }

// List answers after the host's own delay, or not at all if the caller's
// deadline lands first — which is the behaviour the second test measures.
func (o slowOracle) List(ctx context.Context) ([]host.Installed, error) {
	o.host.enter()
	defer o.host.leave()

	timer := time.NewTimer(o.host.wait)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (o slowOracle) Validate(context.Context, string) ([]string, error) {
	return nil, host.ErrNotSupported
}

// TestDoctorProbesHostsInParallel pins the wall clock. A sweep that adds the
// hosts' times together is a diagnostic that takes as long as its slowest
// member multiplied by the number of hosts, and the only slow host is the one
// nobody is waiting for.
func TestDoctorProbesHostsInParallel(t *testing.T) {
	Convey("Given four hosts whose oracles each take 200ms", t, func() {
		slows := []*slowHost{
			newSlowHost(host.Claude, 200*time.Millisecond),
			newSlowHost(host.Codex, 200*time.Millisecond),
			newSlowHost(host.Gemini, 200*time.Millisecond),
			newSlowHost(host.Omp, 200*time.Millisecond),
		}

		adapters := make([]host.Host, 0, len(slows))
		for _, s := range slows {
			adapters = append(adapters, s)
		}

		w := newWorld(t)
		w.hosts = adapters
		w.chdir(t, w.root)

		start := time.Now()

		_, err := w.run("doctor")
		elapsed := time.Since(start)

		So(err, ShouldBeNil)

		Convey("Then the whole run costs one host's wait, not four", func() {
			// Four at 200ms each is 800ms if they queue; overlapped it is
			// ~200ms. The bound is loose enough not to be a timing test and
			// tight enough to fail a sequential sweep on any CI box.
			So(elapsed, ShouldBeLessThan, 600*time.Millisecond)
		})

		Convey("And every host was probed at the same time", func() {
			for _, s := range slows {
				So(s.peakProbes(), ShouldEqual, 1)
				So(s.probeCalls(), ShouldEqual, 1)
			}
		})
	})
}

// TestDoctorStopsAtItsOwnBudget pins the ceiling. One host that never answers
// must not spend the library's full service-backed wait inside a command whose
// entire job is to report.
func TestDoctorStopsAtItsOwnBudget(t *testing.T) {
	Convey("Given a host whose oracle never answers", t, func() {
		stalled := newSlowHost(host.OpenCode, time.Hour)

		w := newWorld(t)
		w.hosts = []host.Host{stalled}
		w.chdir(t, w.root)

		start := time.Now()

		stdout, err := w.run("doctor")
		elapsed := time.Since(start)

		Convey("Then doctor returns on its own deadline, not the oracle's 15s", func() {
			// The run does fail: a host that does not answer is a real
			// finding, and doctor is not in the business of hiding one. What
			// is measured is that it failed on its own budget.
			So(err, ShouldNotBeNil)
			So(elapsed, ShouldBeLessThan, 10*time.Second)
			So(stdout, ShouldContainSubstring, "host:opencode")
		})
	})
}

// TestDoctorFixDoesNotProbeHostsTwice pins the second half of the finding.
// `--fix` re-reads the machine to describe it as it now is, and the host
// section is a statement about hosts: nothing the fix does can change it, so
// probing it again only doubles the slowest part of the run.
func TestDoctorFixDoesNotProbeHostsTwice(t *testing.T) {
	Convey("Given a run with --fix", t, func() {
		counted := newSlowHost(host.Claude, 0)

		w := newWorld(t)
		w.hosts = []host.Host{counted}
		w.chdir(t, w.root)

		_, err := w.run("doctor", "--fix", "-y")
		So(err, ShouldBeNil)

		Convey("Then the host's oracle was asked once, not twice", func() {
			So(counted.probeCalls(), ShouldEqual, 1)
		})
	})
}
