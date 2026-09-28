package verger

import (
	"errors"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// RefusalKind is the small vocabulary a second front end branches on instead of
// matching on error text (DESIGN §9.1, GAP-7). Every refusal verger can produce
// answers with one of these; the message is for the user, the kind is for the
// caller.
type RefusalKind string

// The refusal kinds.
const (
	// RefusalNone is the zero value: the operation was not refused.
	RefusalNone RefusalKind = ""
	// RefusalNeedsConsent is a run that would write hooks or resolve a
	// destructive conflict without a Confirmer (DESIGN §9.1: ErrNeedsConsent).
	RefusalNeedsConsent RefusalKind = "needs-consent"
	// RefusalUntrusted is a project-scoped write the trust gate refused.
	RefusalUntrusted RefusalKind = "untrusted-project"
	// RefusalHostUnavailable is a request naming a host this build cannot
	// deliver to, or one the machine does not detect.
	RefusalHostUnavailable RefusalKind = "host-unavailable"
	// RefusalSchemaNewer is a persisted document this build cannot read
	// (ErrSchemaNewer): the other front end is older than the file.
	RefusalSchemaNewer RefusalKind = "schema-newer"
	// RefusalLocked is a lock or a watch lease another holder owns (ErrLocked).
	RefusalLocked RefusalKind = "locked"
	// RefusalHandsOff is a cell whose artifact changed outside verger and was
	// left in place (ErrHandsOff).
	RefusalHandsOff RefusalKind = "hands-off"
	// RefusalUsage is a caller mistake: an unknown flag value, a malformed ref.
	RefusalUsage RefusalKind = "usage"
	// RefusalNotAvailable is a capability this build does not offer yet.
	RefusalNotAvailable RefusalKind = "not-available"
	// RefusalCheck is a check-only run whose result would change the lock.
	RefusalCheck RefusalKind = "check"
)

// RefusalError is the kind of one refusal plus the message a user reads. A caller
// that only needs the kind calls Classify and never looks at the text.
type RefusalError struct {
	Kind    RefusalKind
	Message string
	Cause   error
}

// Error implements error.
func (e *RefusalError) Error() string { return e.Message }

// Unwrap returns the underlying cause.
func (e *RefusalError) Unwrap() error { return e.Cause }

// RefusalOf returns the refusal an error carries, and whether it is one. Every
// error the facade returns that is a caller-visible refusal is wrapped in one,
// so a second front end can branch on the kind alone.
func RefusalOf(err error) (*RefusalError, bool) {
	return errors.AsType[*RefusalError](err)
}

// Classify names the refusal an error is, whether the facade wrapped it or the
// error came from one of the packages it drives. It is the one call a second
// front end needs: it covers the errors the facade does not construct itself —
// a schema too new to read, a lock another process holds, a hands-off key the
// executor refused — so no branch is ever made on text.
func Classify(err error) RefusalKind {
	if refusal, ok := RefusalOf(err); ok {
		return refusal.Kind
	}

	if errors.Is(err, apply.ErrConfirmationRequired) {
		return RefusalNeedsConsent
	}

	if errors.Is(err, ErrNeedsConsent) {
		return RefusalNeedsConsent
	}

	if _, ok := errors.AsType[*UntrustedProjectError](err); ok {
		return RefusalUntrusted
	}

	if _, ok := errors.AsType[*HostUnavailableError](err); ok {
		return RefusalHostUnavailable
	}

	if _, ok := errors.AsType[*NotAvailableError](err); ok {
		return RefusalNotAvailable
	}

	if _, ok := errors.AsType[*UsageError](err); ok {
		return RefusalUsage
	}

	if _, ok := errors.AsType[*CheckFailedError](err); ok {
		return RefusalCheck
	}

	for _, candidate := range []any{
		&spec.SchemaNewerError{},
		&consent.SchemaNewerError{},
		&home.LockedError{},
		&render.HandsOffError{},
	} {
		if refusalOfShape(err, candidate) {
			return RefusalForShape(candidate)
		}
	}

	return RefusalNone
}

// refusalOfShape reports whether err is of the same concrete type as probe.
func refusalOfShape(err error, probe any) bool {
	switch probe.(type) {
	case *spec.SchemaNewerError:
		_, ok := errors.AsType[*spec.SchemaNewerError](err)

		return ok
	case *consent.SchemaNewerError:
		_, ok := errors.AsType[*consent.SchemaNewerError](err)

		return ok
	case *home.LockedError:
		_, ok := errors.AsType[*home.LockedError](err)

		return ok
	case *render.HandsOffError:
		_, ok := errors.AsType[*render.HandsOffError](err)

		return ok
	default:
		return false
	}
}

// RefusalForShape names the refusal kind of one of the shapes Classify probes
// for, so both live in one table.
func RefusalForShape(probe any) RefusalKind {
	switch probe.(type) {
	case *spec.SchemaNewerError, *consent.SchemaNewerError:
		return RefusalSchemaNewer
	case *home.LockedError:
		return RefusalLocked
	case *render.HandsOffError:
		return RefusalHandsOff
	default:
		return RefusalNone
	}
}

// ErrNeedsConsent reports a run that would write without a Confirmer. It is the
// facade's name for the condition DESIGN §9.1 lists as ErrNeedsConsent, and it
// unwraps apply's own sentinel so a caller may match either.
var ErrNeedsConsent = apply.ErrConfirmationRequired
