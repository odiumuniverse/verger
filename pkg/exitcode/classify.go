package exitcode

import (
	"errors"
	"strings"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/id"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/pack"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/service"
	"github.com/odiumuniverse/verger/pkg/source"
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
// Classified is an error that knows its own exit class.
//
// It exists for the errors declared in packages this file must not import.
// `pkg/cli` sits ABOVE the facade and `cmd/verger` classifies what a command
// returns, so exitcode cannot import it without pointing the dependency the
// wrong way — and the mistake would not show up as a compile error in the
// library, only as a cycle the moment a command called Classify itself. So a
// package whose errors are classified here says so on the type instead.
//
// The table below stays the default for everything else. This is not a way to
// skip the table: every type still has to classify to something the user can act
// on, and classified_gws_test.go still holds each one to that.
type Classified interface {
	error

	ExitClass() int
}

func Classify(err error) int {
	if err == nil {
		return OK
	}

	// Self-declared first. An error that says what it is cannot be improved on
	// by a row that disagrees with it.
	if classified, ok := errors.AsType[Classified](err); ok {
		return classified.ExitClass()
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
	_, receiptNewer := errors.AsType[*receipt.SchemaNewerError](err)

	return consentNewer || storeNewer || lockNewer || specNewer || receiptNewer
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

	// A question the tool could not ask is consent, not a disagreement on disk:
	// the user's next action is to answer it or pass -y, and reporting it as
	// a conflict told a script the run failed for a reason it cannot act on.
	if errors.Is(err, apply.ErrConfirmationRequired) {
		return true
	}

	// The CLI's own wrapper around an untrusted project manifest: a question
	// the user has not answered, and the only way out is to answer it.
	// Hooks waiting on an approval are a question nobody answered, the same
	// shape as -y never reaching one: the user's next action is to answer it.
	if _, ok := errors.AsType[*verger.HookApprovalError](err); ok {
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

	// "This host cannot do that" is a rule refusing the operation, not a host
	// that is missing and not something on disk that disagrees. None of them is
	// fixable by retrying or by editing a file, which is what makes them policy
	// rather than conflict.
	_, notSupported := errors.AsType[*host.NotSupportedError](err)
	_, badStrategy := errors.AsType[*host.UnsupportedStrategyError](err)
	_, badVariable := errors.AsType[*host.UnsupportedVariableError](err)
	_, sourceUnsupported := errors.AsType[*source.NotSupportedError](err)
	_, refusal := errors.AsType[*verger.RefusalError](err)
	// A value that cannot be expressed in the target file is refused, not
	// unread: nothing is wrong with the value, the dialect has no spelling.
	_, inexpressible := errors.AsType[*render.InexpressibleError](err)

	return inHook || notSupported || badStrategy || badVariable ||
		sourceUnsupported || refusal || inexpressible
}

// isConflict is a disagreement the user has to settle.
//
// It is split across helpers because the members are numerous and share no
// logic beyond "one of these matched". A single flat chain was already over the
// complexity limit, and grouping by what they mean is the only way it stays
// readable as more are added — a list nobody can scan is a list nobody checks.
func isConflict(err error) bool {
	_, collision := errors.AsType[*host.CollisionError](err)
	_, restore := errors.AsType[*store.RestoreConflictError](err)
	_, destExists := errors.AsType[*fsutil.DestinationExistsError](err)
	_, srcMissing := errors.AsType[*fsutil.SourceMissingError](err)
	_, contentConflict := errors.AsType[*verger.ContentConflictError](err)
	_, ejectFailed := errors.AsType[*verger.EjectError](err)
	_, checkFailed := errors.AsType[*verger.CheckFailedError](err)

	return collision || restore || destExists || srcMissing ||
		contentConflict || ejectFailed || checkFailed ||
		isLocked(err) || isNamedTwice(err) || isLeftAlone(err) || isCorruptState(err) || isUnreadableContent(err)
}

// isLocked is something else holding the machine: the home's own lock, a lease
// taken by another process, or the run lock. The user's next action is to wait
// or to find out who holds it, which is the same shape every time.
func isLocked(err error) bool {
	_, locked := errors.AsType[*home.LockedError](err)
	_, leased := errors.AsType[*home.LeaseHeldError](err)
	_, applyLock := errors.AsType[*apply.LockError](err)

	return locked || leased || applyLock
}

// isNamedTwice is a spec that names one package twice. Two entries disagree
// about one package and only the user can say which of them is meant.
func isNamedTwice(err error) bool {
	_, duplicateID := errors.AsType[*spec.DuplicateIDError](err)

	return duplicateID
}

// isLeftAlone is a cell verger declined to touch.
//
// A file that is the user's own, a move that would clobber, a render that found
// the content changed: nothing is broken, and the way out is theirs to choose
// (`--force` keeps their copy). Answering anything else let a script read "done"
// over work that was deliberately not done.
func isLeftAlone(err error) bool {
	_, handsOff := errors.AsType[*verger.HandsOffError](err)
	_, renderHandsOff := errors.AsType[*render.HandsOffError](err)
	_, badOwner := errors.AsType[*home.InvalidOwnerError](err)
	_, badMove := errors.AsType[*home.InvalidMoveError](err)
	_, badMoveFs := errors.AsType[*fsutil.InvalidMoveError](err)
	_, homeDestExists := errors.AsType[*home.DestinationExistsError](err)
	_, applyConfig := errors.AsType[*apply.ConfigError](err)
	_, applyReceipt := errors.AsType[*apply.ReceiptError](err)
	_, artifactsMissing := errors.AsType[*apply.ArtifactsMissingError](err)

	return handsOff || renderHandsOff || badOwner || badMove || badMoveFs || homeDestExists ||
		applyConfig || applyReceipt || artifactsMissing
}

// isCorruptState is a persisted document this build will not act on: a receipt,
// a tombstone, a trash, a manifest. Something is on disk and verger cannot use
// it, so the only way forward is the user deciding what that file should be.
func isCorruptState(err error) bool {
	_, corruptReceipt := errors.AsType[*receipt.CorruptReceiptError](err)
	_, corruptJournal := errors.AsType[*receipt.CorruptJournalError](err)
	_, corruptTombstones := errors.AsType[*receipt.CorruptTombstonesError](err)
	_, invalidReceipt := errors.AsType[*receipt.InvalidReceiptError](err)
	_, invalidTombstone := errors.AsType[*receipt.InvalidTombstoneError](err)
	_, invalidReceiptKey := errors.AsType[*receipt.InvalidKeyError](err)
	_, receiptKeyClash := errors.AsType[*receipt.KeyCollisionError](err)
	_, invalidEvent := errors.AsType[*receipt.InvalidEventError](err)
	_, corruptTrash := errors.AsType[*store.CorruptTrashError](err)
	_, trashEntryMissing := errors.AsType[*store.NotFoundError](err)
	_, manifestClash := errors.AsType[*manifest.MergeConflictError](err)
	_, manifestParse := errors.AsType[*manifest.ParseError](err)
	_, piPackage := errors.AsType[*manifest.PiPackageError](err)

	return corruptReceipt || corruptJournal || corruptTombstones || invalidReceipt ||
		invalidTombstone || invalidReceiptKey || receiptKeyClash || invalidEvent ||
		corruptTrash || trashEntryMissing || manifestClash || manifestParse || piPackage
}

// isUnreadableContent is a document verger could not read or could not produce:
// a digest it cannot hash, a config that does not parse, a package that will not
// render. From the outside they are one thing — the content and this build do
// not agree — and a script can only act on the class, not on which of them it
// was.
func isUnreadableContent(err error) bool {
	_, unreadable := errors.AsType[*digest.UnreadableError](err)
	_, configParse := errors.AsType[*render.ConfigParseError](err)
	_, renderFailed := errors.AsType[*render.RenderError](err)
	_, packRender := errors.AsType[*pack.RenderError](err)
	_, packWrite := errors.AsType[*pack.PackWriteError](err)
	_, consentParse := errors.AsType[*consent.ConsentParseError](err)

	return unreadable || configParse || renderFailed || packRender || packWrite || consentParse ||
		isUnreadableState(err)
}

// isUnreadableState is the same answer from the documents verger keeps rather
// than the ones it renders: a lock it cannot use, a consent store in the wrong
// shape, a secret that does not parse.
func isUnreadableState(err error) bool {
	_, consentSchema := errors.AsType[*consent.SchemaInvalidError](err)
	_, secretParse := errors.AsType[*secret.SecretParseError](err)
	_, recordsParse := errors.AsType[*hostcli.RecordsParseError](err)
	_, invalidCell := errors.AsType[*lock.InvalidCellError](err)
	_, invalidLock := errors.AsType[*lock.InvalidLockError](err)
	_, lockSchema := errors.AsType[*lock.SchemaInvalidError](err)
	_, specValue := errors.AsType[*spec.InvalidValueError](err)
	_, specSchema := errors.AsType[*spec.SchemaInvalidError](err)

	return consentSchema || secretParse || recordsParse || invalidCell || invalidLock ||
		lockSchema || specValue || specSchema
}

// isHostUnavailable is something the run needed that could not be used.
//
// It covers two families that look different and answer the same way. A host
// that ran the operation and declined it, and something the run depended on
// being absent or unreachable: no home, no source that answers, no tool, no
// platform underneath. Neither is a disagreement the user settles by editing a
// file, and neither is verger failing to understand itself.
func isHostUnavailable(err error) bool {
	_, facade := errors.AsType[*verger.HostUnavailableError](err)
	_, notAvailable := errors.AsType[*verger.NotAvailableError](err)
	// A watch-limit exhaustion is the closest thing to an unavailable host
	// verger can report: the platform will not give it another watch, so no
	// further work on that host can happen.
	_, limit := errors.AsType[*watch.LimitError](err)
	// The host ran it and said no.
	_, refused := errors.AsType[*apply.HostRefusedError](err)
	_, delivery := errors.AsType[*host.DeliveryError](err)
	_, oracle := errors.AsType[*host.OracleError](err)
	_, cliExit := errors.AsType[*hostcli.ExitError](err)

	return facade || notAvailable || limit || refused || delivery || oracle || cliExit ||
		isNothingToUse(err)
}

// isNothingToUse is the second family: the dependency was not there.
func isNothingToUse(err error) bool {
	_, noHome := errors.AsType[*home.NoHomeError](err)
	_, badHome := errors.AsType[*home.InvalidHomeError](err)
	_, homeSrcMissing := errors.AsType[*home.SourceMissingError](err)
	_, openFailed := errors.AsType[*verger.OpenError](err)
	_, fetch := errors.AsType[*source.FetchError](err)
	_, ref := errors.AsType[*source.RefError](err)
	_, channel := errors.AsType[*source.ChannelError](err)
	_, pinMismatch := errors.AsType[*source.PinMismatchError](err)
	_, toolMissing := errors.AsType[*source.ToolMissingError](err)
	_, noSystemd := errors.AsType[*service.NoUserSystemdError](err)
	_, badPlatform := errors.AsType[*service.UnsupportedPlatformError](err)

	return noHome || badHome || homeSrcMissing || openFailed || fetch || ref || channel ||
		pinMismatch || toolMissing || noSystemd || badPlatform
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
	_, specBadID := errors.AsType[*spec.InvalidIDError](err)
	_, undeclared := errors.AsType[*verger.UndeclaredSourceError](err)
	_, unknownBackend := errors.AsType[*secret.UnknownBackendError](err)
	cliUsage := strings.HasPrefix(err.Error(), "usage:")

	return usage || unknownID || badID || badStoreID || badProject || notInSpec || temporaryHome ||
		specBadID || undeclared || unknownBackend || cliUsage
}
