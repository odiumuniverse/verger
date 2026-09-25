package secret

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Keychain sentinels.
var (
	// ErrKeyringUnavailable reports that the platform keyring tool is missing.
	ErrKeyringUnavailable = errors.New("keyring is unavailable")
	// ErrKeyringUnsupported reports that the platform has no keyring mapping.
	ErrKeyringUnsupported = errors.New("keyring is not supported on this platform")
	// ErrCorruptPayload reports a malformed value stored in the keychain.
	ErrCorruptPayload = errors.New("unexpected keyring payload")
)

const (
	darwinTool    = "security"
	linuxTool     = "secret-tool"
	darwinMissing = 44
	linuxMissing  = 1
	payloadPrefix = "v1:"
)

// Runner executes the platform keyring tool.
type Runner interface {
	Run(name string, args []string, stdin []byte) (stdout []byte, code int, err error)
}

// ExecRunner runs the keyring tool as a child process.
type ExecRunner struct{}

// Run executes name with args and stdin, returning stdout and the exit code.
func (ExecRunner) Run(name string, args []string, stdin []byte) ([]byte, int, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // G204: only the platform keyring tool is executed
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return stdout.Bytes(), exitErr.ExitCode(), fmt.Errorf("%s: %w: %s", name, exitErr, strings.TrimSpace(stderr.String()))
		}

		return stdout.Bytes(), -1, fmt.Errorf("%s: %w", name, err)
	}

	return stdout.Bytes(), 0, nil
}

// Keyring reads, writes and deletes named keychain entries.
type Keyring interface {
	Get(account string) (string, bool, error)
	Set(account, value string) error
	Delete(account string) (bool, error)
}

// shellKeyring talks to security(1) on darwin and secret-tool(1) on linux.
type shellKeyring struct {
	runner   Runner
	tool     string
	service  string
	notFound int
	lookPath func(string) (string, error)
}

// keyringPlatform maps a GOOS to its tool and missing-entry exit code.
func keyringPlatform(goos string) (tool string, missingCode int, ok bool) {
	switch goos {
	case "darwin":
		return darwinTool, darwinMissing, true
	case "linux":
		return linuxTool, linuxMissing, true
	default:
		return "", 0, false
	}
}

// NewShellKeyring returns the platform keyring; service is the keychain
// service name ("verger" standalone, "beadle" when embedded in the vault).
func NewShellKeyring(r Runner, service string) (Keyring, error) {
	tool, notFound, ok := keyringPlatform(runtime.GOOS)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrKeyringUnsupported, runtime.GOOS)
	}

	if service == "" {
		service = DefaultService
	}

	return &shellKeyring{
		runner:   r,
		tool:     tool,
		service:  service,
		notFound: notFound,
		lookPath: exec.LookPath,
	}, nil
}

// probeAccount is the reserved account of a keychain probe.
func probeAccount(service string) string {
	return service + "-doctor-probe"
}

// available reports ErrKeyringUnavailable when the tool is not on PATH.
func (k *shellKeyring) available() error {
	if _, err := k.lookPath(k.tool); err != nil {
		return fmt.Errorf("%w: %s not found in PATH", ErrKeyringUnavailable, k.tool)
	}

	return nil
}

// Get reads account; a missing entry reports found=false without error.
func (k *shellKeyring) Get(account string) (string, bool, error) {
	if err := k.available(); err != nil {
		return "", false, err
	}

	stdout, code, err := k.runner.Run(k.tool, k.getArgs(account), nil)

	switch {
	case code == k.notFound:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("keyring get %s: %w", account, err)
	case code != 0:
		return "", false, fmt.Errorf("keyring get %s: exit code %d", account, code)
	}

	return k.decodeValue(account, stdout)
}

// decodeValue turns tool stdout into a value, rejecting corrupt payloads.
func (k *shellKeyring) decodeValue(account string, stdout []byte) (string, bool, error) {
	raw := strings.TrimSuffix(string(stdout), "\n")

	value, err := decodePayload(raw)
	if err != nil {
		return "", false, fmt.Errorf("keyring get %s: %w", account, err)
	}

	return value, true, nil
}

// Set writes account, never exposing the value in argv on darwin.
func (k *shellKeyring) Set(account, value string) error {
	if err := k.available(); err != nil {
		return err
	}

	args, stdin := k.setCommand(account, value)

	_, code, err := k.runner.Run(k.tool, args, stdin)
	if err == nil && code == 0 {
		return nil
	}

	if err == nil {
		err = fmt.Errorf("exit code %d", code)
	}

	return fmt.Errorf("keyring set %s: %w", account, err)
}

// Delete removes account, proving the removal with a read-back.
func (k *shellKeyring) Delete(account string) (bool, error) {
	if err := k.available(); err != nil {
		return false, err
	}

	_, code, err := k.runner.Run(k.tool, k.deleteArgs(account), nil)
	if err != nil && code != k.notFound {
		return false, fmt.Errorf("keyring delete %s: %w", account, err)
	}

	if code != 0 && code != k.notFound {
		return false, fmt.Errorf("keyring delete %s: exit code %d", account, code)
	}

	_, found, err := k.Get(account)
	if err != nil {
		return false, fmt.Errorf("keyring delete %s: read-back: %w", account, err)
	}

	if found {
		return false, fmt.Errorf("keyring delete %s: the entry is still present", account)
	}

	return code == 0, nil
}

// getArgs renders the lookup argv of the platform tool.
func (k *shellKeyring) getArgs(account string) []string {
	if k.tool == linuxTool {
		return []string{"lookup", "service", k.service, "account", account}
	}

	return []string{"find-generic-password", "-s", k.service, "-a", account, "-w"}
}

// setCommand renders the store argv and stdin of the platform tool.
func (k *shellKeyring) setCommand(account, value string) ([]string, []byte) {
	if k.tool == linuxTool {
		args := []string{"store", "--label=" + k.service + ": " + account, "service", k.service, "account", account}

		return args, []byte(encodePayload(value))
	}

	args := []string{"add-generic-password", "-U", "-s", k.service, "-a", account, "-X", hex.EncodeToString([]byte(encodePayload(value)))}

	return args, nil
}

// deleteArgs renders the delete argv of the platform tool.
func (k *shellKeyring) deleteArgs(account string) []string {
	if k.tool == linuxTool {
		return []string{"clear", "service", k.service, "account", account}
	}

	return []string{"delete-generic-password", "-s", k.service, "-a", account}
}

// encodePayload marks a value as verger-owned: v1:<hex>.
func encodePayload(value string) string {
	return payloadPrefix + hex.EncodeToString([]byte(value))
}

// decodePayload reverses encodePayload; foreign raw values pass through.
func decodePayload(raw string) (string, error) {
	encoded, ok := strings.CutPrefix(raw, payloadPrefix)
	if !ok {
		return raw, nil
	}

	value, err := hex.DecodeString(encoded)
	if err != nil {
		return "", ErrCorruptPayload
	}

	return string(value), nil
}
