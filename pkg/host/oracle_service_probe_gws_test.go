package host_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
)

// Kilo carries its own oracle constant, so `WithOracleWait` — the seam every
// other host honours, and the only way a caller keeps a test from sitting
// through the real bound — did nothing for it. A test that shortened the wait
// to keep itself quick still waited the full thirty seconds, and a caller that
// gave the whole sweep a shared budget still handed kilo a budget of its own.
func TestKiloOracleHonoursTheInjectedWait(t *testing.T) {
	Convey("Given a caller that shortens the oracle wait", t, func() {
		// The shim is what makes this test independent of whether kilo is
		// installed on the machine running it. Without it the runner never
		// runs and the note is "host CLI not found" — a different failure,
		// and one that passes for the wrong reason wherever kilo happens to
		// be installed.
		fakeKilo(t)

		kilo, _ := newKilo(t, t.TempDir(), nil,
			host.WithRunner(blockingRunner{}),
			host.WithOracleWait(150*time.Millisecond),
		)

		start := time.Now()

		_, err := kilo.Oracle().List(t.Context())
		elapsed := time.Since(start)

		Convey("Then the oracle ends on the injected wait, not its own 30s", func() {
			So(err, ShouldNotBeNil)
			So(elapsed, ShouldBeLessThan, 5*time.Second)
		})

		Convey("And the note names the wait that actually applied", func() {
			So(err.Error(), ShouldContainSubstring, "did not answer within")
			So(err.Error(), ShouldNotContainSubstring, "30s")
		})
	})
}

// serviceAwareRunner answers the verbs that do not touch the host's
// background service and blocks on the ones that do, which is the shape of a
// machine where the binary is installed and the service is not running.
type serviceAwareRunner struct {
	answers map[string]hostcli.Response
	blockOn []string

	sawVersion bool
}

func (r *serviceAwareRunner) Run(
	ctx context.Context, bin hostcli.Binary, args []string, _ []byte,
) ([]byte, error) {
	verb := strings.Join(args, " ")

	if slices.Contains(r.blockOn, verb) {
		<-ctx.Done()

		return nil, ctx.Err()
	}

	if verb == "--version" {
		r.sawVersion = true
	}

	if answer, ok := r.answers[verb]; ok {
		return answer.Stdout, answer.Err
	}

	return nil, nil
}

// The plugin listing talks to a service the CLI starts itself. When only that
// verb fails, "did not answer within 3s" describes a symptom and sends the
// reader to wait longer; the cause is that nothing is listening, and the fix
// is to start it.
func TestOpenCodeSaysTheServiceIsNotListening(t *testing.T) {
	Convey("Given a binary that answers, and a background service that does not", t, func() {
		runner := &serviceAwareRunner{blockOn: []string{"plugin list"}}

		open := host.NewOpenCode(
			host.WithHome(t.TempDir()),
			host.WithRunner(runner),
			host.WithOracleWait(200*time.Millisecond),
		)

		_, err := open.Oracle().List(t.Context())

		Convey("Then the note names the service rather than the timeout", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "service is not listening")
			So(runner.sawVersion, ShouldBeTrue)
		})

		Convey("And it still says how to start it", func() {
			So(err.Error(), ShouldContainSubstring, "opencode service start")
		})
	})
}

// A binary that does not answer at all is a different problem, and saying
// "the service is not listening" about it would point at the wrong thing.
func TestOpenCodeDistinguishesAWedgedBinaryFromADeadService(t *testing.T) {
	Convey("Given a binary that answers nothing", t, func() {
		open := host.NewOpenCode(
			host.WithHome(t.TempDir()),
			host.WithRunner(blockingRunner{}),
			host.WithOracleWait(200*time.Millisecond),
		)

		_, err := open.Oracle().List(t.Context())

		Convey("Then the note does not blame the service", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldNotContainSubstring, "service is not listening")
		})
	})
}
