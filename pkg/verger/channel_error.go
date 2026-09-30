package verger

import (
	"context"
	"errors"

	"github.com/odiumuniverse/verger/pkg/source"
)

// A channel the user wrote is part of the invocation, so a channel that does
// not resolve is a usage error and exits 2 - the same code a misspelled flag
// gets. `pkg/source` cannot say that itself: it is below the facade and knows
// nothing about exit codes, and its own errors carry no classification. This is
// the one place the two meet.
//
// It is also the only thing standing between a mistyped channel and a silent
// fallback. The product decision was explicit: no channel, or no such channel,
// is refused with the list of what does exist. Reporting it as anything other
// than a usage error would put it under a different exit code, and a script
// that treats "conflict" as retryable would retry a typo forever.

// fetchResolved fetches one ref and classifies the failures that are really
// about how the run was invoked.
func fetchResolved(ctx context.Context, fetcher *source.Fetcher, ref source.Ref) (*source.Fetched, error) {
	got, err := fetcher.Fetch(ctx, ref)
	if err != nil {
		return nil, classifyChannelError(err)
	}

	return got, nil
}

// classifyChannelError turns a source channel failure into a usage error and
// leaves every other error exactly as it was: a ref that will not fetch
// because the network is down is not a usage error, and re-labelling it would
// tell the user to fix their command line.
func classifyChannelError(err error) error {
	if _, ok := errors.AsType[*source.ChannelError](err); !ok {
		return err
	}

	return &UsageError{Cause: err}
}
