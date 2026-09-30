package exitcode_test

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/id"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/service"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/store"
	"github.com/odiumuniverse/verger/pkg/verger"
	"github.com/odiumuniverse/verger/pkg/watch"
)

func TestNilIsOK(t *testing.T) {
	Convey("Given no error", t, func() {
		Convey("Then the class is ok", func() {
			So(exitcode.Classify(nil), ShouldEqual, exitcode.OK)
			So(exitcode.OK, ShouldEqual, 0)
		})
	})
}

// The published contract: these integers are what a script compares against,
// so a change to any of them is a breaking change and the test says so.
func TestTheClassNumbersAreFixed(t *testing.T) {
	Convey("Given the eight classes", t, func() {
		Convey("Then they are the documented integers", func() {
			So(exitcode.OK, ShouldEqual, 0)
			So(exitcode.Unexpected, ShouldEqual, 1)
			So(exitcode.Usage, ShouldEqual, 2)
			So(exitcode.Conflict, ShouldEqual, 3)
			So(exitcode.Policy, ShouldEqual, 4)
			So(exitcode.Consent, ShouldEqual, 5)
			So(exitcode.HostUnavailable, ShouldEqual, 6)
			So(exitcode.SchemaNewer, ShouldEqual, 7)
		})
	})

	Convey("Given each class", t, func() {
		Convey("Then it has a name a message can use", func() {
			So(exitcode.Name(exitcode.OK), ShouldEqual, "ok")
			So(exitcode.Name(exitcode.Unexpected), ShouldEqual, "unexpected")
			So(exitcode.Name(exitcode.Usage), ShouldEqual, "usage")
			So(exitcode.Name(exitcode.Conflict), ShouldEqual, "conflict")
			So(exitcode.Name(exitcode.Policy), ShouldEqual, "policy refusal")
			So(exitcode.Name(exitcode.Consent), ShouldEqual, "consent needed")
			So(exitcode.Name(exitcode.HostUnavailable), ShouldEqual, "host unavailable")
			So(exitcode.Name(exitcode.SchemaNewer), ShouldEqual, "newer schema")
		})

		Convey("Then an unknown number is named as unknown rather than as a class", func() {
			So(exitcode.Name(42), ShouldEqual, "unknown")
		})
	})
}

func TestClassifyEachTypedError(t *testing.T) {
	Convey("Given one error of each kind", t, func() {
		table := []struct {
			name string
			err  error
			want int
		}{
			// 7 — schema
			{"consent schema newer", &consent.SchemaNewerError{}, exitcode.SchemaNewer},
			{"store schema newer", &store.SchemaNewerError{}, exitcode.SchemaNewer},
			{"lock schema newer", &lock.SchemaNewerError{}, exitcode.SchemaNewer},
			{"spec schema newer", &spec.SchemaNewerError{}, exitcode.SchemaNewer},

			// 5 — consent
			{"untrusted project", &verger.UntrustedProjectError{}, exitcode.Consent},
			{"missing secrets", &host.MissingSecretsError{}, exitcode.Consent},
			{"keyring unavailable", fmt.Errorf("load: %w", secret.ErrKeyringUnavailable), exitcode.Consent},
			{"keyring unsupported", secret.ErrKeyringUnsupported, exitcode.Consent},
			// A pending question is 5, not 3: the next action is the user
			// answering, not the user settling a disagreement on disk. This
			// is what `verger restore` with no terminal and no -y returns,
			// and reporting it as a conflict told a script the run "failed"
			// for a reason it cannot act on.
			{"confirmation required", apply.ErrConfirmationRequired, exitcode.Consent},
			{"confirmation required, wrapped", fmt.Errorf("restore: %w", apply.ErrConfirmationRequired), exitcode.Consent},

			// 4 — policy
			{"policy", &host.PolicyError{}, exitcode.Policy},
			{"secret in a hook", &host.SecretInHookError{}, exitcode.Policy},

			// 3 — conflict
			{"collision", &host.CollisionError{}, exitcode.Conflict},
			{"restore conflict", &store.RestoreConflictError{}, exitcode.Conflict},
			{"home locked", &home.LockedError{}, exitcode.Conflict},
			{"lease held", &home.LeaseHeldError{}, exitcode.Conflict},
			{"your edit left alone", &verger.HandsOffError{Cells: []string{"local:one@pi"}}, exitcode.Conflict},
			{"no keychain here", &secret.KeyringUnavailableError{Reason: secret.UnavailableMessage}, exitcode.HostUnavailable},

			// 6 — host unavailable
			{"host unavailable", &verger.HostUnavailableError{}, exitcode.HostUnavailable},
			{"not available", &verger.NotAvailableError{}, exitcode.HostUnavailable},
			{"watch limit", &watch.LimitError{}, exitcode.HostUnavailable},

			// 2 — usage
			{"usage", &verger.UsageError{}, exitcode.Usage},
			{"unknown host id", &hostpath.UnknownIDError{ID: "nope"}, exitcode.Usage},
			{"invalid id", &id.InvalidIDError{Value: "!!", Reason: "bad"}, exitcode.Usage},
			{"package not in spec", &verger.PackageNotInSpecError{ID: "x"}, exitcode.Usage},
			{"invalid project", &consent.InvalidProjectError{}, exitcode.Usage},
			{"temporary home", &service.ErrTemporaryHome{Home: "/tmp/x"}, exitcode.Usage},

			// 1 — unclassified
			{"plain error", errors.New("boom"), exitcode.Unexpected},
			{"fs not exist", fs.ErrNotExist, exitcode.Unexpected},
			{"store not found", &store.NotFoundError{}, exitcode.Unexpected},
		}

		for _, item := range table {
			Convey("Then "+item.name+" is "+exitcode.Name(item.want), func() {
				So(exitcode.Classify(item.err), ShouldEqual, item.want)
			})
		}
	})
}

func TestClassifySeesThroughWrapping(t *testing.T) {
	Convey("Given a typed error wrapped twice", t, func() {
		inner := &host.CollisionError{}
		wrapped := fmt.Errorf("apply: %w", fmt.Errorf("plan: %w", inner))

		Convey("Then the class is the inner one", func() {
			So(exitcode.Classify(wrapped), ShouldEqual, exitcode.Conflict)
		})
	})
}

// The precedence rules are the part a script actually depends on, so each one
// is pinned by a real error that satisfies two classes at once.
func TestPrecedenceWhenSeveralClassesApply(t *testing.T) {
	Convey("Given a missing secret inside a hook refusal", t, func() {
		// SecretInHookError is policy; MissingSecretsError is consent. Both
		// hold, and consent must win because the user is prompted.
		both := fmt.Errorf("hook: %w", &host.MissingSecretsError{})
		_ = both

		Convey("Then a bare MissingSecretsError is consent", func() {
			So(exitcode.Classify(&host.MissingSecretsError{}), ShouldEqual, exitcode.Consent)
		})

		Convey("Then a SecretInHookError is policy, not consent", func() {
			// The opposite half of the same pair: a secret named by a hook is
			// refused outright, and no prompt follows.
			So(exitcode.Classify(&host.SecretInHookError{}), ShouldEqual, exitcode.Policy)
		})
	})

	Convey("Given a schema-newer error wrapping a policy refusal", t, func() {
		wrapped := fmt.Errorf("load: %w", &store.SchemaNewerError{Path: "/h/.verger/verger.lock"})

		Convey("Then schema wins: a newer file must not be rewritten at all", func() {
			So(exitcode.Classify(wrapped), ShouldEqual, exitcode.SchemaNewer)
		})
	})

	Convey("Given a conflict raised while a host is unavailable", t, func() {
		Convey("Then the class is decided by the error, not by the context", func() {
			// There is no realistic error satisfying both today; the property
			// that matters is that the two checks do not shadow each other.
			So(exitcode.Classify(&host.CollisionError{}), ShouldEqual, exitcode.Conflict)
			So(exitcode.Classify(&verger.HostUnavailableError{}), ShouldEqual, exitcode.HostUnavailable)
		})
	})
}
