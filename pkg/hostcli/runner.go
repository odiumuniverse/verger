package hostcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

// Runner executes a resolved binary. Every Ф1 package talks to hosts through
// it; pkg/host tests inject ScriptRunner.
type Runner interface {
	Run(ctx context.Context, bin Binary, args []string, stdin []byte) (stdout []byte, err error)
}

// StreamRunner is a Runner that also returns what the child wrote to stderr
// when it succeeds: some host CLIs print their results there (Gemini CLI 0.61
// prints `extensions list` output, JSON included, to stderr).
type StreamRunner interface {
	RunStreams(ctx context.Context, bin Binary, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// ExecRunner runs the resolved binary as a child process, never through a
// shell: arguments are passed verbatim.
type ExecRunner struct{}

// Run executes bin with args and stdin. A canceled context surfaces ctx.Err();
// a binary that vanished before it ran is ErrNotFound; one that ran and exited
// non-zero is an *ExitError carrying its code and stderr.
func (r ExecRunner) Run(ctx context.Context, bin Binary, args []string, stdin []byte) ([]byte, error) {
	stdout, _, err := r.RunStreams(ctx, bin, args, stdin)

	return stdout, err
}

// RunStreams implements StreamRunner with the semantics of Run.
func (ExecRunner) RunStreams(ctx context.Context, bin Binary, args []string, stdin []byte) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", bin.Name, err)
	}

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, bin.Path, args...) //nolint:gosec // G204: only a resolved host CLI runs, without a shell
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if bin.PATH != "" {
		cmd.Env = withPATH(os.Environ(), bin.PATH)
	}

	err := cmd.Run()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w", bin.Name, ctxErr)
	}

	switch exitErr, exited := errors.AsType[*exec.ExitError](err); {
	case err == nil:
		return stdout.Bytes(), stderr.Bytes(), nil
	case exited:
		return stdout.Bytes(), stderr.Bytes(), &ExitError{Name: bin.Name, Code: exitErr.ExitCode(), Stderr: stderr.String(), Err: exitErr}
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, exec.ErrNotFound):
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w: %w", bin.Name, ErrNotFound, err)
	default:
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w", bin.Name, err)
	}
}

// RunWith runs one binary through an explicit runner; a nil runner uses
// ExecRunner. A canceled context surfaces ctx.Err() before the runner starts.
func (b Binary) RunWith(ctx context.Context, r Runner, args []string, stdin []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", b.Name, err)
	}

	if r == nil {
		r = ExecRunner{}
	}

	return r.Run(ctx, b, args, stdin)
}

// RunStreamsWith is RunWith returning stderr as well: through the runner's
// StreamRunner when it has one, else stderr is nil (the runner cannot report
// it). A nil runner uses ExecRunner.
func (b Binary) RunStreamsWith(ctx context.Context, r Runner, args []string, stdin []byte) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", b.Name, err)
	}

	if r == nil {
		r = ExecRunner{}
	}

	if streams, ok := r.(StreamRunner); ok {
		return streams.RunStreams(ctx, b, args, stdin)
	}

	stdout, err := r.Run(ctx, b, args, stdin)

	return stdout, nil, err
}

// Run is RunWith with ExecRunner (kept for beadle parity).
func (b Binary) Run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	return b.RunWith(ctx, ExecRunner{}, args, stdin)
}

// Version returns the first line the binary prints for --version.
func (b Binary) Version(ctx context.Context) (string, error) {
	out, err := b.Run(ctx, []string{"--version"}, nil)
	if err != nil {
		return "", err
	}

	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")

	return strings.TrimSpace(line), nil
}

// ScriptRunner is a scripted Runner for tests and CI fixtures. It is safe for
// concurrent use.
type ScriptRunner struct {
	mu        sync.Mutex
	responses map[string]Response
	calls     []Call
}

// Response is one scripted answer of a ScriptRunner.
type Response struct {
	Stdout []byte
	Stderr string
	Code   int
	Err    error
}

// Call is one recorded invocation of a ScriptRunner.
type Call struct {
	Binary string
	Args   []string
	Stdin  []byte
}

// NewScriptRunner builds a runner scripted by `<name> <joined args>` keys; an
// unscripted call fails loudly with *ExitError{Code: 127}.
func NewScriptRunner(script map[string]Response) *ScriptRunner {
	responses := make(map[string]Response, len(script))

	maps.Copy(responses, script)

	return &ScriptRunner{responses: responses}
}

// Run implements Runner.
func (s *ScriptRunner) Run(ctx context.Context, bin Binary, args []string, stdin []byte) ([]byte, error) {
	stdout, _, err := s.RunStreams(ctx, bin, args, stdin)

	return stdout, err
}

// RunStreams implements StreamRunner: the scripted Stderr is returned on
// success too, like a CLI that prints its results there.
func (s *ScriptRunner) RunStreams(ctx context.Context, bin Binary, args []string, stdin []byte) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", bin.Name, err)
	}

	key := scriptKey(bin.Name, args)

	s.mu.Lock()
	s.calls = append(s.calls, Call{Binary: bin.Name, Args: slices.Clone(args), Stdin: bytes.Clone(stdin)})
	response, ok := s.responses[key]
	s.mu.Unlock()

	stderr := []byte(response.Stderr)

	switch {
	case !ok:
		return nil, nil, &ExitError{Name: bin.Name, Code: 127, Stderr: "no scripted response"}
	case response.Err != nil:
		return bytes.Clone(response.Stdout), stderr, response.Err
	case response.Code != 0:
		return bytes.Clone(response.Stdout), stderr, &ExitError{Name: bin.Name, Code: response.Code, Stderr: response.Stderr}
	default:
		return bytes.Clone(response.Stdout), stderr, nil
	}
}

// Calls returns a copy of every recorded call, including stdin.
func (s *ScriptRunner) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Call, len(s.calls))

	for i, call := range s.calls {
		out[i] = Call{Binary: call.Binary, Args: slices.Clone(call.Args), Stdin: bytes.Clone(call.Stdin)}
	}

	return out
}

// Set scripts one more response, replacing any previous one for the key.
func (s *ScriptRunner) Set(key string, resp Response) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.responses == nil {
		s.responses = map[string]Response{}
	}

	s.responses[key] = resp
}

// scriptKey renders the `<name> <joined args>` key of one call.
func scriptKey(name string, args []string) string {
	if len(args) == 0 {
		return name
	}

	return name + " " + strings.Join(args, " ")
}

// withPATH returns env with PATH replaced.
func withPATH(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)

	for _, entry := range env {
		if !strings.HasPrefix(entry, "PATH=") {
			out = append(out, entry)
		}
	}

	return append(out, "PATH="+path)
}
