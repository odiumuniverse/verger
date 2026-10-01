package exitcode_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/exitcode"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/cli"
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

// Exit class 1 means exactly one thing: verger does not know what happened.
// Every error verger raises on purpose is a condition it can name, so it belongs
// in a class the user can act on — a bad package id, a newer lock, a host that
// would not do what it was asked.
//
// The classifier answers that with a table, and a table decays silently: an
// error type added later matches no row and falls through to 1. Nothing else in
// the suite notices. A failed cell on `remove` sat unclassified for a whole
// release — the run printed "the agent kept the plugin" and exited 1, which is
// the code that means verger itself broke.
//
// So this does not trust the table. It walks it: every error type verger raises
// is classified, and a type with no row fails here by name, which is the row
// that is missing.
//
// Zero values are the right instances. Classify matches on type and never reads
// contents, so a zero value exercises what a real one would — and building real
// ones would mean every type needs a constructor that exists only for this test.
var raisedErrors = []struct {
	where string
	err   error
}{
	{"apply.ArtifactsMissingError", &apply.ArtifactsMissingError{}},
	{"apply.ConfigError", &apply.ConfigError{}},
	{"apply.HostRefusedError", &apply.HostRefusedError{}},
	{"apply.LockError", &apply.LockError{}},
	{"apply.ReceiptError", &apply.ReceiptError{}},
	{"cli.ApplyFailedError", &cli.ApplyFailedError{}},
	{"cli.LockedError", &cli.LockedError{}},
	{"cli.NotAvailableError", &cli.NotAvailableError{}},
	{"cli.TrustError", &cli.TrustError{}},
	{"cli.UsageError", &cli.UsageError{}},
	{"consent.ConsentParseError", &consent.ConsentParseError{}},
	{"consent.InvalidProjectError", &consent.InvalidProjectError{}},
	{"consent.SchemaInvalidError", &consent.SchemaInvalidError{}},
	{"consent.SchemaNewerError", &consent.SchemaNewerError{}},
	{"digest.UnreadableError", &digest.UnreadableError{}},
	{"fsutil.DestinationExistsError", &fsutil.DestinationExistsError{}},
	{"fsutil.InvalidMoveError", &fsutil.InvalidMoveError{}},
	{"fsutil.SourceMissingError", &fsutil.SourceMissingError{}},
	{"home.DestinationExistsError", &home.DestinationExistsError{}},
	{"home.InvalidHomeError", &home.InvalidHomeError{}},
	{"home.InvalidMoveError", &home.InvalidMoveError{}},
	{"home.InvalidOwnerError", &home.InvalidOwnerError{}},
	{"home.LeaseHeldError", &home.LeaseHeldError{}},
	{"home.LockedError", &home.LockedError{}},
	{"home.NoHomeError", &home.NoHomeError{}},
	{"home.SourceMissingError", &home.SourceMissingError{}},
	{"host.CollisionError", &host.CollisionError{}},
	{"host.DeliveryError", &host.DeliveryError{}},
	{"host.MissingSecretsError", &host.MissingSecretsError{}},
	{"host.NotSupportedError", &host.NotSupportedError{}},
	{"host.OracleError", &host.OracleError{}},
	{"host.PolicyError", &host.PolicyError{}},
	{"host.SecretInHookError", &host.SecretInHookError{}},
	{"host.UnsupportedStrategyError", &host.UnsupportedStrategyError{}},
	{"host.UnsupportedVariableError", &host.UnsupportedVariableError{}},
	{"hostcli.ExitError", &hostcli.ExitError{}},
	{"hostcli.RecordsParseError", &hostcli.RecordsParseError{}},
	{"hostpath.UnknownIDError", &hostpath.UnknownIDError{}},
	{"id.InvalidIDError", &id.InvalidIDError{}},
	{"lock.InvalidCellError", &lock.InvalidCellError{}},
	{"lock.InvalidLockError", &lock.InvalidLockError{}},
	{"lock.SchemaInvalidError", &lock.SchemaInvalidError{}},
	{"lock.SchemaNewerError", &lock.SchemaNewerError{}},
	{"manifest.MergeConflictError", &manifest.MergeConflictError{}},
	{"manifest.ParseError", &manifest.ParseError{}},
	{"manifest.PiPackageError", &manifest.PiPackageError{}},
	{"pack.PackWriteError", &pack.PackWriteError{}},
	{"pack.RenderError", &pack.RenderError{}},
	{"receipt.CorruptJournalError", &receipt.CorruptJournalError{}},
	{"receipt.CorruptReceiptError", &receipt.CorruptReceiptError{}},
	{"receipt.CorruptTombstonesError", &receipt.CorruptTombstonesError{}},
	{"receipt.InvalidEventError", &receipt.InvalidEventError{}},
	{"receipt.InvalidKeyError", &receipt.InvalidKeyError{}},
	{"receipt.InvalidReceiptError", &receipt.InvalidReceiptError{}},
	{"receipt.InvalidTombstoneError", &receipt.InvalidTombstoneError{}},
	{"receipt.KeyCollisionError", &receipt.KeyCollisionError{}},
	{"receipt.SchemaNewerError", &receipt.SchemaNewerError{}},
	{"render.ConfigParseError", &render.ConfigParseError{}},
	{"render.HandsOffError", &render.HandsOffError{}},
	{"render.InexpressibleError", &render.InexpressibleError{}},
	{"render.RenderError", &render.RenderError{}},
	{"secret.KeyringUnavailableError", &secret.KeyringUnavailableError{}},
	{"secret.SecretParseError", &secret.SecretParseError{}},
	{"secret.UnknownBackendError", &secret.UnknownBackendError{}},
	{"service.NoUserSystemdError", &service.NoUserSystemdError{}},
	{"service.UnsupportedPlatformError", &service.UnsupportedPlatformError{}},
	{"source.ChannelError", &source.ChannelError{}},
	{"source.FetchError", &source.FetchError{}},
	{"source.NotSupportedError", &source.NotSupportedError{}},
	{"source.PinMismatchError", &source.PinMismatchError{}},
	{"source.RefError", &source.RefError{}},
	{"source.ToolMissingError", &source.ToolMissingError{}},
	{"spec.DuplicateIDError", &spec.DuplicateIDError{}},
	{"spec.InvalidIDError", &spec.InvalidIDError{}},
	{"spec.InvalidValueError", &spec.InvalidValueError{}},
	{"spec.SchemaInvalidError", &spec.SchemaInvalidError{}},
	{"spec.SchemaNewerError", &spec.SchemaNewerError{}},
	{"store.CorruptTrashError", &store.CorruptTrashError{}},
	{"store.InvalidIDError", &store.InvalidIDError{}},
	{"store.NotFoundError", &store.NotFoundError{}},
	{"store.RestoreConflictError", &store.RestoreConflictError{}},
	{"store.SchemaNewerError", &store.SchemaNewerError{}},
	{"verger.CheckFailedError", &verger.CheckFailedError{}},
	{"verger.ContentConflictError", &verger.ContentConflictError{}},
	{"verger.EjectError", &verger.EjectError{}},
	{"verger.HandsOffError", &verger.HandsOffError{}},
	{"verger.HookApprovalError", &verger.HookApprovalError{}},
	{"verger.HostUnavailableError", &verger.HostUnavailableError{}},
	{"verger.NotAvailableError", &verger.NotAvailableError{}},
	{"verger.OpenError", &verger.OpenError{}},
	{"verger.PackageNotInSpecError", &verger.PackageNotInSpecError{}},
	{"verger.PendingConsentError", &verger.PendingConsentError{}},
	{"verger.RefusalError", &verger.RefusalError{}},
	{"verger.UndeclaredSourceError", &verger.UndeclaredSourceError{}},
	{"verger.UntrustedProjectError", &verger.UntrustedProjectError{}},
	{"verger.UsageError", &verger.UsageError{}},
	{"watch.LimitError", &watch.LimitError{}},
}

func TestEveryErrorVergerRaisesIsClassified(t *testing.T) {
	Convey("Given every error type verger raises on purpose", t, func() {
		Convey("Then each one lands in a class the user can act on, not in 1", func() {
			var unclassified []string

			for _, row := range raisedErrors {
				fillCause(row.err)

				if exitcode.Classify(row.err) == exitcode.Unexpected {
					unclassified = append(unclassified, row.where)
				}
			}

			So(strings.Join(unclassified, " "), ShouldEqual, "")
		})

		Convey("And the table is still the whole set of them", func() {
			So(declaredErrorTypes(), ShouldResemble, tableNames())
		})
	})
}

// tableNames is what the table above claims to cover.
func tableNames() []string {
	out := make([]string, 0, len(raisedErrors))
	for _, row := range raisedErrors {
		out = append(out, row.where)
	}

	return out
}

var errorTypeDecl = regexp.MustCompile(`^type ([A-Z][A-Za-z0-9_]*Error) struct`)

// declaredErrorTypes walks the source for the error types each package declares.
//
// This is what makes the table above self-repairing in the only direction that
// matters. The table is hand-written and can only ever be wrong by omission, and
// an omitted type is exactly the defect: it is classified, silently, as 1. So
// the walk is authoritative and the table has to keep up with it. A new error
// type fails here on the day it is written, while its author still knows why it
// exists.
func declaredErrorTypes() []string {
	var out []string

	root := filepath.Join("..", "..", "pkg")

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // G304: the path comes from walking the repo's own source tree
		if readErr != nil {
			return readErr
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		pkg, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		if pkg == "" || pkg == filepath.ToSlash(rel) {
			return nil
		}

		for line := range strings.Lines(string(data)) {
			if m := errorTypeDecl.FindStringSubmatch(line); m != nil {
				out = append(out, pkg+"."+m[1])
			}
		}

		return nil
	})
	if err != nil {
		panic(err)
	}

	slicesSort(out)

	return out
}

// slicesSort sorts in place; the walk returns them unordered.
func slicesSort(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// sentinelError stands in for a wrapped cause so that formatting a zero value
// returns a string instead of dereferencing nil.
type sentinelError struct{}

// Error implements error.
func (sentinelError) Error() string { return "sentinel" }

var errorField = reflect.TypeFor[error]()

// fillCause makes one of the table's instances safe to classify.
//
// Classify calls err.Error() on at least one branch, and nearly every type here
// formats a wrapped cause, so a bare zero value panics on the nil inside it. The
// guard would then report a crash inside the classifier instead of the missing
// row it went looking for — and a crash aborts the run, so every type after it
// would go unchecked. The sentinel matches nothing in the classifier; it is
// only ever there so that formatting returns.
func fillCause(err error) {
	value := reflect.ValueOf(err)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return
	}

	elem := value.Elem()
	if elem.Kind() != reflect.Struct {
		return
	}

	for _, field := range elem.Fields() {
		if field.CanSet() && field.Type() == errorField {
			field.Set(reflect.ValueOf(sentinelError{}))
		}
	}
}

// The rule the guard cannot express: class 1 is for errors verger did not
// anticipate, and "the host said no" is not one of them. It is named here
// because it is the case that was wrong — a removal reported the agent still
// had the plugin and exited 1, so a script reading that code was told verger
// had crashed when a host had simply answered.
func TestARefusedOperationIsTheHostFailing(t *testing.T) {
	Convey("Given a host that ran the removal and kept the plugin", t, func() {
		err := &apply.HostRefusedError{
			Host:    host.Claude,
			Package: "acme/plugin",
			Action:  "remove",
			Output:  "plugin 1.0.0",
		}

		Convey("Then it is class 6, not the class that means verger broke", func() {
			So(exitcode.Classify(err), ShouldEqual, exitcode.HostUnavailable)
			So(exitcode.Classify(err), ShouldNotEqual, exitcode.Unexpected)
		})

		Convey("And the message names the host and what its CLI printed", func() {
			So(err.Error(), ShouldContainSubstring, "claude")
			So(err.Error(), ShouldContainSubstring, "plugin 1.0.0")
		})
	})
}
