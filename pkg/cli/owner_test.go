package cli

import (
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
)

func TestReceiptOwnerArtifactDigest(t *testing.T) {
	Convey("Given a receipt store with one recorded artifact", t, func() {
		receiptsDir := filepath.Join(t.TempDir(), "receipts")
		store := receipt.NewStore(receiptsDir)
		path := filepath.Join(t.TempDir(), "claude.json#mcpServers.github")
		sum := digest.Bytes([]byte(`{"command":"gh"}`))

		So(store.Put(receipt.Receipt{
			Schema: receipt.Schema, Package: "acme/tool", Host: "claude", Scope: receipt.ScopeUser,
			Strategy: "loose", Version: "1.0.0",
			Artifacts: []receipt.Artifact{{Kind: "mcp", Name: "github", Path: path, Digest: sum}},
		}), ShouldBeNil)

		var owner host.PathOwner = verger.NewOwnership(receiptsDir)

		Convey("When the adapter asks for the recorded digest (NF-2)", func() {
			digests, ok := owner.(host.ArtifactDigests)

			Convey("Then the owner offers the optional extension and returns the receipt digest", func() {
				So(ok, ShouldBeTrue)

				got, found := digests.ArtifactDigest(path)
				So(found, ShouldBeTrue)
				So(got, ShouldEqual, sum)

				_, found = digests.ArtifactDigest(path + ".other")
				So(found, ShouldBeFalse)
			})

			Convey("Then ownership still names the package", func() {
				pkg, owned := owner.Owner(path)
				So(owned, ShouldBeTrue)
				So(pkg, ShouldEqual, "acme/tool")
			})
		})
	})
}
