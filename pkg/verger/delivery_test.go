package verger_test

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// piPayload writes one pi package (a package.json carrying the `pi` key) into a
// temp dir and returns its root.
func piPayload(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	body := `{"name":"pi-thing","version":"1.0.0","pi":{"model":"claude"}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(body), 0o600); err != nil { //nolint:gosec // G306: a test fixture
		t.Fatalf("write package.json: %v", err)
	}

	return root
}

// TestBuildHostPackageRefusesPiPackage pins the plan-time pi-package refusal
// (W3-E2E10 F4): a payload whose only manifest is a pi `package.json` must be
// refused by name at plan time, not fail later in the executor with a message
// about the version. Deleting the PiPackageRefusal call in buildHostPackage
// leaves this test red.
func TestBuildHostPackageRefusesPiPackage(t *testing.T) {
	Convey("Given a client and a pi package payload", t, func() {
		root := piPayload(t)

		client, err := verger.Open(t.Context())
		So(err, ShouldBeNil)

		t.Cleanup(func() { _ = client.Close() })

		fetched := &source.Fetched{
			Ref:  source.Ref{Kind: source.KindLocal, Raw: root, Path: root},
			Root: root,
			Package: &manifest.Package{
				ID: "acme/pi-thing", Version: "1.0.0", Format: manifest.FormatClaude, Root: root,
			},
		}

		Convey("When the package is lifted for a host", func() {
			pkg, err := verger.BuildHostPackage(client, fetched, host.Claude, "", "user", "")

			Convey("Then it is refused by name as a pi package", func() {
				So(err, ShouldNotBeNil)
				So(manifest.IsPiPackageError(err), ShouldBeTrue)
				So(pkg, ShouldResemble, host.Package{})
			})
		})

		Convey("When the payload is not a pi package", func() {
			plain := t.TempDir()
			body := `{"name":"plain","version":"1.0.0"}` + "\n"

			if err := os.WriteFile(filepath.Join(plain, "package.json"), []byte(body), 0o600); err != nil { //nolint:gosec // G306: a test fixture
				t.Fatalf("write package.json: %v", err)
			}

			fetched.Root = plain
			fetched.Package.Root = plain

			pkg, err := verger.BuildHostPackage(client, fetched, host.Claude, "", "user", "")

			Convey("Then it is lifted without a refusal", func() {
				So(err, ShouldBeNil)
				So(pkg.ID, ShouldEqual, "acme/pi-thing")
			})
		})
	})
}
