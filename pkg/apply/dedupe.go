package apply

// DedupeActions drops a repeated install of one cell, keeping the first.
//
// The executor runs every action it is given, so two installs of the same
// package to the same host write the same paths twice. The second run finds
// what the first just wrote; having no receipt of its own to compare that
// against, it reports the cell as restored from the lock — as though nothing
// had been installed on this machine — and a forced run that should have
// overwritten a hand-edited file reports no backup because there was no
// earlier delivery for it to have drifted from.
//
// The duplicate arrives easily: a lock entry and a spec entry that name the
// same package in different spellings both resolve to one id. Only installs
// are collapsed. A removal followed by an install is how a package is
// replaced, and that order is the whole point of the pair.
func DedupeActions(actions []Action) []Action {
	seen := make(map[string]bool, len(actions))
	out := make([]Action, 0, len(actions))

	for _, action := range actions {
		if action.Kind != ActionInstall {
			out = append(out, action)

			continue
		}

		key := string(action.Host) + "|" + action.Delivery.Package.ID
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, action)
	}

	return out
}
