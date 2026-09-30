package verger

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/receipt"
)

// A receipt records a CLI-managed MCP server under the identity
// `<host>://mcp/<name>` — a deliberate address, not a place on disk. Treating
// every artifact path as a filesystem path made `os.Stat` fail on that URI, so
// a gemini delivery that had written every file read as `missing`: the gemini
// e2e failed on both scenarios with "receipt is here but the delivered files
// are not" while `.gemini/` held all of them.
func TestReceiptFilesPresentIgnoresHostSideIdentities(t *testing.T) {
	Convey("Given a receipt whose files are all on disk", t, func() {
		dir := t.TempDir()
		skill := filepath.Join(dir, "skills", "one")
		So(os.MkdirAll(skill, 0o700), ShouldBeNil)
		So(os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("# one\n"), 0o600), ShouldBeNil)

		record := receipt.Receipt{
			Artifacts: []receipt.Artifact{
				{Kind: "skill", Name: "one", Path: skill},
				// The MCP server exists only as an address; the host owns it.
				{Kind: "mcp", Name: "fixture-mcp", Path: "gemini://mcp/fixture-mcp"},
			},
		}

		Convey("Then the receipt reads as present", func() {
			So(ReceiptFilesPresent(record), ShouldBeTrue)
		})
	})

	Convey("Given a receipt whose file really is gone", t, func() {
		dir := t.TempDir()

		record := receipt.Receipt{
			Artifacts: []receipt.Artifact{
				{Kind: "skill", Name: "one", Path: filepath.Join(dir, "skills", "one")},
			},
		}

		Convey("Then it is still missing", func() {
			// The identity rule must not turn the check off for real files.
			So(ReceiptFilesPresent(record), ShouldBeFalse)
		})
	})
}
