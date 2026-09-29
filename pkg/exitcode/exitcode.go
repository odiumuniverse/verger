// Package exitcode is the one source of truth for what a command's exit status
// means. Both CLIs classify their errors through it, so a script that runs
// either tool reads the same number for the same situation.
//
// The classes, and what a script is expected to do with each:
//
//	0  ok                 the command did what it was asked
//	1  unexpected         a bug: something the tool did not anticipate
//	2  usage              bad flags, an unknown host or package id, a missing argument
//	3  conflict           the state on disk disagrees and the user must decide
//	4  policy refusal     a rule forbade it: managed policy, a protected file, a refusal
//	5  consent needed     something waits on the user: hook approval, a keyring prompt
//	6  host unavailable   the agent's own CLI or runtime is not there
//	7  newer schema       the data was written by a newer tool; do not touch it
//
// Only 0 and 1 are "the tool is broken" signals. Everything else is a state the
// user can act on, and a script that only checks `!= 0` is treating a
// recoverable situation as a crash — which is why the classes exist.
//
// # Precedence
//
// When an error satisfies more than one class, the most specific wins, and the
// order is fixed (Classify walks it top to bottom):
//
//	7  newer schema     a newer file must never be rewritten by an older binary,
//	                    whatever else is wrong with it
//	5  consent needed   the user's next action is the same prompt either way, so
//	                    the message that names the prompt must win
//	4  policy refusal   not resolvable by the user; must not be reported as a
//	                    decision they can make
//	3  conflict         a decision the user can make
//	6  host unavailable the host binary is missing, so nothing else can succeed
//	2  usage            the invocation itself is wrong
//	1  unexpected       anything unclassified
//
// Schema first, because it is the only class where acting is actively harmful.
// Consent above policy because both mean "the user must act", and the consent
// message is the more specific one.
//
// # Adding a class
//
// Do not. The numbering is a published contract: a script compares against
// these integers, so 8 is free but renumbering is not. A new situation belongs
// in an existing class, and if none fits it is class 1 until the table is
// revised deliberately.
package exitcode

// The exit classes. These integers are the contract; see the package doc.
const (
	// OK is a command that did what it was asked.
	OK = 0
	// Unexpected is a bug in the tool.
	Unexpected = 1
	// Usage is a bad invocation.
	Usage = 2
	// Conflict is a disagreement the user must resolve.
	Conflict = 3
	// Policy is a refusal by a rule.
	Policy = 4
	// Consent is a prompt waiting on the user.
	Consent = 5
	// HostUnavailable is a missing agent CLI or runtime.
	HostUnavailable = 6
	// SchemaNewer is data written by a newer tool.
	SchemaNewer = 7
)

// Name returns the human name of a class, for a message or a doctor line. An
// unknown number is reported by its number rather than as a name, so a new
// class is visible rather than silently nameless.
func Name(code int) string {
	switch code {
	case OK:
		return "ok"
	case Unexpected:
		return "unexpected"
	case Usage:
		return "usage"
	case Conflict:
		return "conflict"
	case Policy:
		return "policy refusal"
	case Consent:
		return "consent needed"
	case HostUnavailable:
		return "host unavailable"
	case SchemaNewer:
		return "newer schema"
	default:
		return "unknown"
	}
}
