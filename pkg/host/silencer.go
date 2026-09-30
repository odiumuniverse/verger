package host

// Silencer is the optional interface an adapter implements when it has a
// surface for most packages and none at all for some. A package the adapter
// cannot represent — one whose kind it has no layout for, or whose scope it has
// nowhere to put — is not a delivery that should fail: the ladder's answer is
// to record the cell as silenced, with the reason and the hint, so the user
// learns what would have to change instead of reading an error from a stratum
// that was never going to be asked to do the work.
//
// It is deliberately **not** part of Host. Most adapters cannot answer the
// question, and folding it into Host would force every one of them to answer
// "yes, I can deliver that" for packages they cannot, turning an absent
// capability into an asserted lie. Callers discover it by type assertion, so
// an adapter that has no opinion is never asked.
//
// The three return values are the contract: ok false means "no opinion", and
// the caller falls through to the ordinary ladder. When ok is true, reason is
// what the user is told and hint is the thing they would do about it — a
// reason without a hint is a dead end, and a hint without a reason is advice
// about a problem the user was never shown.
type Silencer interface {
	// Silence reports whether this adapter cannot deliver pkg at all, and why.
	Silence(pkg Package, scope string) (reason, hint string, ok bool)
}
