package verger

import (
	"strings"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// applyChannels copies the spec's channel names onto the refs that name the
// same package. The ref is the thing that gets fetched, so the channel has to
// arrive with it: a caller that fetched first and resolved second would fetch
// whatever the ref pointed at and only then go looking for the channel, which
// is the one moment where resolving cannot help.
//
// A ref the spec does not mention keeps an empty channel and is fetched as
// written. A ref carrying a channel the source cannot serve is not corrected
// here - Fetch refuses it, because "stable" on a local directory is a question
// the user can only answer by editing the spec, not something to guess at.
func applyChannels(sp *spec.Spec, refs []source.Ref) {
	if sp == nil {
		return
	}

	byID := make(map[string]string, len(sp.Packages))

	for _, entry := range sp.Packages {
		if entry.Channel != "" {
			byID[entry.ID] = entry.Channel
		}
	}

	if len(byID) == 0 {
		return
	}

	for i := range refs {
		if channel, ok := byID[entryID(refs[i])]; ok {
			refs[i].Channel = channel
		}
	}
}

// entryID reports the id a ref is filed under in the spec. A ref that names a
// version is filed under the package, not under the pin: `pkg@1.2.3` and `pkg`
// are the same entry, and a spec that wrote one of them must still set the
// channel of the other.
func entryID(ref source.Ref) string {
	if bareID(ref) {
		return ref.ID
	}

	if at := strings.LastIndex(ref.Raw, "@"); at > 0 {
		return ref.Raw[:at]
	}

	return ref.Raw
}

// channelOf reports the channel one spec entry names, or "" when it names none.
// A disabled entry still carries its channel: whether the entry is installed
// this run is a different question from which version it would install as.
func channelOf(sp *spec.Spec, id string) string {
	if sp == nil {
		return ""
	}

	for _, entry := range sp.Packages {
		if entry.ID == id {
			return entry.Channel
		}
	}

	return ""
}
