package verger

import (
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// Ownership resolves path ownership from a receipt store. It is the
// host.PathOwner and host.ArtifactDigests implementation every delivery passes
// to its adapters, so a re-delivery can prove that a path or an MCP server is
// this package's before it replaces it.
type Ownership struct {
	receipts *receipt.Store
}

// NewOwnership returns the receipt-backed ownership source of one receipts
// directory. A second front end that drives pkg/apply itself injects the same
// source Install and Remove use.
func NewOwnership(receiptsDir string) Ownership {
	return Ownership{receipts: receipt.NewStore(receiptsDir)}
}

// Owner implements host.PathOwner.
func (o Ownership) Owner(path string) (string, bool) {
	record, _, ok := o.artifact(path)

	return record.Package, ok
}

// ArtifactDigest implements host.ArtifactDigests: an unchanged MCP server is
// re-delivered without a host call (NF-2).
func (o Ownership) ArtifactDigest(path string) (digest.Hash, bool) {
	_, artifact, ok := o.artifact(path)

	return artifact.Digest, ok
}

// artifact finds the receipt and artifact recorded for one path.
func (o Ownership) artifact(path string) (receipt.Receipt, receipt.Artifact, bool) {
	list, err := o.receipts.List()
	if err != nil {
		return receipt.Receipt{}, receipt.Artifact{}, false
	}

	for _, record := range list {
		for _, artifact := range record.Artifacts {
			if artifact.Path == path {
				return record, artifact, true
			}
		}
	}

	return receipt.Receipt{}, receipt.Artifact{}, false
}
