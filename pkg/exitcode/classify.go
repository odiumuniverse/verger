package exitcode

import (
	"errors"
	"strings"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/fsutil"
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

// Classify maps an error to its exit class, or OK for a nil error.
//
// It works through wrapping: a command may return fmt.Errorf("...: %w", err)
// and the type is still found, so a caller does not have to keep the chain
// itself.
//
// # Dependency direction — read before adding a type here
//
// This package imports the facade package `pkg/verger`, because three of the
// most important errors (UsageError, HostUnavailableError, UntrustedProjectError)
// are declared there and classifying them as "unexpected" would defeat the
// point. That means nothing under pkg/verger may import this package. Call it
// from cmd/ and from pkg/cli, which are already above the facade — never from
// pkg/verger itself, which would be an import cycle.
func Classify(err error) int {
	if err == nil {
		return OK
	}

	switch {
	// 6 — no keyring here. First, because the typed error unwraps to the
	// older keyring sentinels, which read as a consent question. It is not
	// one: nothing is waiting for an answer, the machine simply has nowhere
	// to put a secret, and the sentence says so.
	case isKeyringUnavailable(err):
		return HostUnavailable

	// 7 — schema. After that, because a newer file must never be rewritten by
	// an older binary whatever else is wrong with the invocation.
	case isNewerSchema(err):
		return SchemaNewer

	// 5 — consent. Above policy because the user's next action is a prompt
	// either way, and this is the message that names it.
	case isConsent(err):
		return Consent

	// 4 — policy. Not resolvable by the user, so it must never be reported as
	// a decision they can make.
	case isPolicy(err):
		return Policy

	// 3 — conflict. A disagreement on disk that only the user can settle.
	case isConflict(err):
		return Conflict

	// 6 — host unavailable. Below conflict on purpose: a missing host is a
	// reason, not the only reason, a command can fail.
	case isHostUnavailable(err):
		return HostUnavailable

	// 2 — usage. The invocation itself is wrong; nothing on disk is at stake.
	case isUsage(err):
		return Usage

	default:
		return Unexpected
	}
}

// isNewerSchema reports a persisted document written by a newer tool. Every
// document verger reads has its own SchemaNewerError, so all of them are
// named here: a lock or a spec left by a newer verger must classify as 7 on
// every command, not only the ones that happen to touch consent or receipts.
func isNewerSchema(err error) bool {
	_, consentNewer := errors.AsType[*consent.SchemaNewerError](err)
	_, storeNewer := errors.AsType[*store.SchemaNewerError](err)
	_, lockNewer := errors.AsType[*lock.SchemaNewerError](err)
	_, specNewer := errors.AsType[*spec.SchemaNewerError](err)

	return consentNewer || storeNewer || lockNewer || specNewer
}

// isKeyringUnavailable is the typed "this machine has nowhere to keep a
// secret" answer. It is a distinct case from the keyring sentinels, which
// still mean "a secret was needed and could not be supplied" and keep their
// consent exit.
func isKeyringUnavailable(err error) bool {
	_, typed := errors.AsType[*secret.KeyringUnavailableError](err)

	return typed
}

func isConsent(err error) bool {
	// A secret named by a hook cannot be stored in a settings file: the hook
	// would be refused, and only the user can change what the package asks for.
	if _, inHook := errors.AsType[*host.SecretInHookError](err); inHook {
		return false
	}

	// An untrusted project manifest is a consent question, not a policy one:
	// the tool is waiting for the user to say "yes, I trust this".
	if _, ok := errors.AsType[*verger.UntrustedProjectError](err); ok {
		return true
	}

	// A package needs a secret the vault has not got.
	if _, ok := errors.AsType[*host.MissingSecretsError](err); ok {
		return true
	}

	return errors.Is(err, secret.ErrKeyringUnavailable) ||
		errors.Is(err, secret.ErrKeyringUnsupported)
}

func isPolicy(err error) bool {
	if _, ok := errors.AsType[*host.PolicyError](err); ok {
		return true
	}

	// Checked separately from MissingSecretsError because the two are opposites:
	// a missing secret waits for the user, a secret in a hook is refused.
	_, inHook := errors.AsType[*host.SecretInHookError](err)

	return inHook
}

func isConflict(err error) bool {
	_, collision := errors.AsType[*host.CollisionError](err)
	_, restore := errors.AsType[*store.RestoreConflictError](err)
	_, locked := errors.AsType[*home.LockedError](err)
	_, leased := errors.AsType[*home.LeaseHeldError](err)
	_, applyLock := errors.AsType[*apply.LockError](err)
	_, destExists := errors.AsType[*fsutil.DestinationExistsError](err)
	_, srcMissing := errors.AsType[*fsutil.SourceMissingError](err)
	// A cell verger refused to write because the file is the user's own is a
	// conflict, not a failure: nothing is broken, and the way out is theirs to
	// choose (`--force` keeps their copy and overwrites). Answering anything
	// else let a script read "done" over a file that was deliberately left
	// alone.
	_, handsOff := errors.AsType[*verger.HandsOffError](err)
	confirm := errors.Is(err, apply.ErrConfirmationRequired)

	return collision || restore || locked || leased || applyLock || destExists || srcMissing || confirm || handsOff
}

func isHostUnavailable(err error) bool {
	_, facade := errors.AsType[*verger.HostUnavailableError](err)
	_, notAvailable := errors.AsType[*verger.NotAvailableError](err)
	// A watch-limit exhaustion is the closest thing to an unavailable host
	// verger can report: the platform will not give it another watch, so no
	// further work on that host can happen.
	_, limit := errors.AsType[*watch.LimitError](err)

	return facade || notAvailable || limit
}

func isUsage(err error) bool {
	if err == nil {
		return false
	}

	_, usage := errors.AsType[*verger.UsageError](err)
	_, unknownID := errors.AsType[*hostpath.UnknownIDError](err)
	_, badID := errors.AsType[*id.InvalidIDError](err)
	_, badStoreID := errors.AsType[*store.InvalidIDError](err)
	_, badProject := errors.AsType[*consent.InvalidProjectError](err)
	_, notInSpec := errors.AsType[*verger.PackageNotInSpecError](err)
	// A login service pinned to a temporary home is a wrong invocation, not a
	// conflict and not a crash: the user pointed the command at a home that
	// cannot outlive the unit. It belongs with the other "you asked for
	// something impossible" shapes, so a script gets 2 and can fix the call.
	_, temporaryHome := errors.AsType[*service.ErrTemporaryHome](err)
	cliUsage := strings.HasPrefix(err.Error(), "usage:")

	return usage || unknownID || badID || badStoreID || badProject || notInSpec || temporaryHome || cliUsage
}
