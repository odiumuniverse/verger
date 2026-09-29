package apply_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// fakeProbe is a scripted apply.DriftProbe.
type fakeProbe struct {
	sum     digest.Hash
	gone    bool
	unknown bool
	err     error
}

// MCPDigest implements apply.DriftProbe.
func (p fakeProbe) MCPDigest(context.Context, string) (apply.MCPDrift, error) {
	return apply.MCPDrift{Sum: p.sum, Gone: p.gone, Unknown: p.unknown}, p.err
}

// mcpReceipt builds one receipt claiming one CLI-managed MCP server.
func mcpReceipt(host, name string, sum digest.Hash) receipt.Receipt {
	return receipt.Receipt{
		Package: "acme/drift",
		Host:    host,
		Scope:   "user",
		Artifacts: []receipt.Artifact{
			{Kind: "mcp", Name: name, Path: host + "://mcp/" + name, Digest: sum},
		},
		RMA: []receipt.Op{
			{Kind: receipt.OpHostInstall, Command: []string{"claude", "mcp", "remove", name, "-s", "user"}},
		},
	}
}

func TestReceiptDriftMCP(t *testing.T) {
	ctx := context.Background()
	sum := digest.Bytes([]byte(`{"Name":"probe","Transport":"stdio","Command":["node","/tmp/probe.js"],"Env":null,"URL":"","Headers":null}`))

	Convey("Given a receipt claiming one CLI-managed MCP server", t, func() {
		record := mcpReceipt("claude", "probe", sum)

		Convey("When the host reports the same value", func() {
			report, err := apply.ReceiptDrift(ctx, record, fakeProbe{sum: sum})

			Convey("Then the cell is not drift", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeFalse)
			})
		})

		Convey("When the host reports an edited command", func() {
			edited := digest.Bytes([]byte(`{"Name":"probe","Transport":"stdio","Command":["node","/usr/local/bin/attacker"],"Env":null,"URL":"","Headers":null}`))
			report, err := apply.ReceiptDrift(ctx, record, fakeProbe{sum: edited})

			Convey("Then the cell is drift and names the server", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeTrue)
				So(report.Keys, ShouldResemble, []string{"claude://mcp/probe"})
				So(report.Note(), ShouldEqual, "claude://mcp/probe changed outside verger")
			})
		})

		Convey("When the host no longer has the server", func() {
			report, err := apply.ReceiptDrift(ctx, record, fakeProbe{gone: true})

			Convey("Then the cell is drift", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeTrue)
			})
		})

		Convey("When the host cannot be asked", func() {
			report, err := apply.ReceiptDrift(ctx, record, fakeProbe{unknown: true})

			Convey("Then the cell is not drift", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeFalse)
			})
		})

		Convey("When the probe fails", func() {
			_, err := apply.ReceiptDrift(ctx, record, fakeProbe{err: errors.New("boom")})

			Convey("Then the error surfaces", func() {
				So(err, ShouldNotBeNil)
			})
		})

		Convey("When there is no probe", func() {
			report, err := apply.ReceiptDrift(ctx, record, nil)

			Convey("Then the cell is not drift", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeFalse)
			})
		})
	})

	Convey("Given a receipt with no MCP artifact", t, func() {
		record := receipt.Receipt{
			Package: "acme/drift", Host: "claude", Scope: "user",
			RMA: []receipt.Op{{Kind: receipt.OpConfigKey, Path: "/x.json", KeyPath: "hooks", Digest: sum}},
		}

		Convey("When a probe is supplied", func() {
			report, err := apply.ReceiptDrift(ctx, record, fakeProbe{gone: true})

			Convey("Then the probe is never consulted", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeFalse)
			})
		})
	})
}

// TestReceiptDriftRecord pins DRIFT-2: ReceiptDrift walks OpRecord ops
// per-record, so a hand-edited hook record is reported as drift on that
// record, not on the whole document.
func TestReceiptDriftRecord(t *testing.T) {
	ctx := context.Background()

	hooksDoc := `{
  "hooks": {
    "beforeShellExecution": [
      {"command": "verger run", "timeout": 30}
    ],
    "afterFileEdit": [
      {"command": "verger lint", "timeout": 10}
    ]
  }
}`

	// Compute digests for each record
	rec1Digest := digest.Bytes([]byte(`{"command":"verger run","timeout":30}`))
	rec2Digest := digest.Bytes([]byte(`{"command":"verger lint","timeout":10}`))

	receipt := receipt.Receipt{
		Package: "acme/hooks",
		Host:    "cursor",
		Scope:   "user",
		Artifacts: []receipt.Artifact{
			{Kind: "hook-record", Name: "beforeShellExecution/verger run", Path: "cursor://hooks/beforeShellExecution/verger run", Digest: rec1Digest},
			{Kind: "hook-record", Name: "afterFileEdit/verger lint", Path: "cursor://hooks/afterFileEdit/verger lint", Digest: rec2Digest},
		},
		RMA: []receipt.Op{
			{Kind: receipt.OpRecord, Path: "/home/user/.cursor/hooks.json", Note: "hooks.beforeShellExecution[0]", Digest: rec1Digest},
			{Kind: receipt.OpRecord, Path: "/home/user/.cursor/hooks.json", Note: "hooks.afterFileEdit[0]", Digest: rec2Digest},
		},
	}

	Convey("Given a receipt with two hook records", t, func() {
		dir := t.TempDir()
		hooksPath := dir + "/.cursor/hooks.json"

		if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(hooksPath, []byte(hooksDoc), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		Convey("When ReceiptDrift walks the receipt", func() {
			// Fix the receipt to point at the real file
			receipt.RMA[0].Path = hooksPath
			receipt.RMA[1].Path = hooksPath

			report, err := apply.ReceiptDrift(ctx, receipt, nil)

			Convey("Then no drift is reported when records match", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeFalse)
			})
		})

		Convey("When one record is edited by hand", func() {
			editedDoc := `{
  "hooks": {
    "beforeShellExecution": [
      {"command": "verger run", "timeout": 30}
    ],
    "afterFileEdit": [
      {"command": "attacker command", "timeout": 10}
    ]
  }
}`
			if err := os.WriteFile(hooksPath, []byte(editedDoc), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			receipt.RMA[0].Path = hooksPath
			receipt.RMA[1].Path = hooksPath

			report, err := apply.ReceiptDrift(ctx, receipt, nil)

			Convey("Then only the edited record is reported as drift", func() {
				So(err, ShouldBeNil)
				So(report.Drifted(), ShouldBeTrue)
				So(report.Keys, ShouldHaveLength, 1)
				So(report.Keys[0], ShouldContainSubstring, "afterFileEdit")
			})
		})
	})
}
