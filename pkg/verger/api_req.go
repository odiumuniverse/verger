package verger

import (
	"context"
	"errors"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

var (
	errNoSuchPackage = errors.New("no such package")
	errNoSuchHost    = errors.New("no such host")
)

// EjectError reports a home move that did not complete: the target could not
// be opened, or the move itself failed. A caller matches it with errors.Is or
// errors.As and never reads the message.
type EjectError struct {
	// Target is the home the client tried to move to.
	Target string
	// Cause is the underlying failure.
	Cause error
}

// Error implements error.
func (e *EjectError) Error() string { return "eject: " + e.Cause.Error() }

// Unwrap returns the underlying cause.
func (e *EjectError) Unwrap() error { return e.Cause }

// Eject moves this client's home to a fresh empty target home. It is the
// package-level Eject bound to a client, so a front end calls it without
// opening a second client by hand. Every failure is an *EjectError.
func (c *Client) Eject(ctx context.Context, target string) error {
	to, err := Open(ctx, WithHome(target))
	if err != nil {
		return &EjectError{Target: target, Cause: err}
	}

	defer func() { _ = to.Close() }()

	if _, err := Eject(ctx, c, to); err != nil {
		return &EjectError{Target: target, Cause: err}
	}

	return nil
}

// HookApprovalError reports a hook approval that was not recorded: the consent
// store could not be opened, the approval was refused, or it could not be
// persisted. A caller matches it with errors.Is or errors.As and never reads
// the message.
type HookApprovalError struct {
	// Package is the package the approval was for.
	Package string
	// Host is the host the approval was for.
	Host string
	// Cause is the underlying failure.
	Cause error
}

// Error implements error.
func (e *HookApprovalError) Error() string { return "approve hooks: " + e.Cause.Error() }

// Unwrap returns the underlying cause.
func (e *HookApprovalError) Unwrap() error { return e.Cause }

// ApproveHooksFor records an explicit approval for a package's hooks on a
// host, by content hash, and returns the key the approval is stored under.
// Every failure is a *HookApprovalError.
func (c *Client) ApproveHooksFor(pkgID, host string, hash digest.Hash) (string, error) {
	store, err := c.ConsentStore()
	if err != nil {
		return "", &HookApprovalError{Package: pkgID, Host: host, Cause: err}
	}

	if err := store.ApproveHooks(pkgID, "", hash); err != nil {
		return "", &HookApprovalError{Package: pkgID, Host: host, Cause: err}
	}

	if err := store.Save(); err != nil {
		return "", &HookApprovalError{Package: pkgID, Host: host, Cause: err}
	}

	return pkgID, nil
}

// Unregister removes a package entry from the spec, leaving its receipts and
// installed files alone; Uninstall removes those too.
func (c *Client) Unregister(ctx context.Context, paths Paths, id string, dryRun bool) error {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	if !hasSpecPackageID(doc, id) {
		return &UsageError{Cause: errNoSuchPackage}
	}

	if dryRun {
		_, err := c.Outdated(ctx, StatusOptions{})
		return err
	}

	RemoveSpecPackage(doc, id)

	return SaveSpec(paths.SpecPath, doc)
}

// UnregisterHost runs the inverse RMA of a host-native registration verger
// did not create itself (beadle's old directory marketplace), so beadle
// never unregisters by hand.
func (c *Client) UnregisterHost(ctx context.Context, hostID, marketplace string) error {
	adapters := c.Hosts()

	var adapter host.Host

	for _, a := range adapters {
		if string(a.ID()) == hostID {
			adapter = a
			break
		}
	}

	if adapter == nil {
		return &UsageError{Cause: errNoSuchHost}
	}

	receipts := receipt.NewStore(c.Home().ReceiptsDir())

	list, err := receipts.List()
	if err != nil {
		return err
	}

	for _, record := range list {
		if record.Host != hostID || record.Package == "" {
			continue
		}

		if _, err := adapter.Uninstall(ctx, "", record); err != nil {
			return err
		}
	}

	return nil
}
