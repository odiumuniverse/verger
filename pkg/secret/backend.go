package secret

import "fmt"

// BackendAuto picks the keyring when a probe says there is one, and the file
// backend otherwise. It is the default, and it never fails.
const BackendAuto = "auto"

// BackendRequest is one backend selection. Probe is required: there is no
// default that quietly means "assume the keyring is there", because that
// assumption is what ends in an OS dialog.
type BackendRequest struct {
	// Want is BackendAuto, BackendKeyring or BackendFile.
	Want string
	// Dir is where the file backend keeps its values.
	Dir string
	// Service is the keychain service name ("verger", or "beadle" when
	// embedded).
	Service string
	// Probe answers whether a keyring can be used here.
	Probe KeyringProbe
	// Shell builds the platform keyring, once the probe has said yes. It is
	// a field so a test can hand over a fake and never reach the real store.
	Shell func(runner Runner, service string) (Keyring, error)
	// Runner executes the platform keyring tool; nil means ExecRunner.
	Runner Runner
}

// UnknownBackendError reports a request verger cannot satisfy.
type UnknownBackendError struct{ Name string }

// Error implements error.
func (e *UnknownBackendError) Error() string {
	return fmt.Sprintf("unknown secret backend %q: use %q, %q or %q",
		e.Name, BackendAuto, BackendKeyring, BackendFile)
}

// SelectBackend returns the keyring to use and the name of the backend it is.
// The probe runs before anything is constructed, so no code path can reach a
// keyring that was never checked — which is the only way to guarantee the OS
// never offers to reset anything.
func SelectBackend(req BackendRequest) (Keyring, string, error) {
	switch req.Want {
	case "", BackendAuto:
		if req.Probe != nil {
			ok, _ := req.Probe.Available()
			if ok {
				kr, err := buildKeyring(req)

				return kr, BackendKeyring, err
			}
		}

		return fileBackend(req)

	case BackendKeyring:
		if err := RequireKeyring(req.Probe); err != nil {
			return nil, "", err
		}

		kr, err := buildKeyring(req)

		return kr, BackendKeyring, err

	case BackendFile:
		return fileBackend(req)

	default:
		return nil, "", &UnknownBackendError{Name: req.Want}
	}
}

// fileBackend builds the file keyring, or reports why it cannot.
func fileBackend(req BackendRequest) (Keyring, string, error) {
	if req.Dir == "" {
		return nil, "", fmt.Errorf("%w: the file backend needs a directory", ErrKeyringUnsupported)
	}

	kr, err := NewFileKeyring(req.Dir)
	if err != nil {
		return nil, "", err
	}

	return kr, BackendFile, nil
}

// buildKeyring constructs the platform keyring after the probe said yes.
func buildKeyring(req BackendRequest) (Keyring, error) {
	build := req.Shell
	if build == nil {
		build = NewShellKeyring
	}

	runner := req.Runner
	if runner == nil {
		runner = ExecRunner{}
	}

	return build(runner, req.Service)
}
