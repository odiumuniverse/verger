// Package spec models verger.toml (desired state) for the user and project
// scopes: parsing, validation, deterministic marshalling with unknown-field
// round-trip, scope merge, propagation resolution and the trust digest.
package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	pkgid "github.com/odiumuniverse/verger/pkg/id"
)

// Schema is the newest spec schema version understood by this package.
const Schema = 1

// DefaultCooldown is the upstream update delay applied when a spec does not
// set one.
const DefaultCooldown = 24 * time.Hour

// Fixed schema key names reused across extraction, validation and merge.
const (
	defaultsTable = "defaults"
	packageTable  = "package"
	cooldownKey   = "cooldown"
)

// Keys of the typed schema, used to separate unknown fields from known ones.
// They are fixed-size arrays: compile-time read-only tables that are never
// mutated or appended to (decision T0.5 N2).
var (
	topKeys       = [...]string{"schema", defaultsTable, "propagate", "source", packageTable}
	defaultsKeys  = [...]string{"hooks", cooldownKey}
	propagateKeys = [...]string{
		"install", "remove", "disable", "enable", "update", "adopt", "marketplace", "kind", "host",
	}
	sourceKeys  = [...]string{"name", "url"}
	packageKeys = [...]string{
		"id", "channel", "except", "env", "version", "disabled", "adopted_from", "propagate",
	}
)

// Spec is the desired state of one verger.toml document.
type Spec struct {
	Schema    int       `toml:"schema"`
	Defaults  Defaults  `toml:"defaults,omitempty"`
	Propagate Propagate `toml:"propagate,omitempty"`
	// Hosts is the per-host feature switch table (U2): every behaviour is on by
	// default, and a host switches one off without touching any other host.
	Hosts    map[string]HostSettings `toml:"hosts,omitempty"`
	Sources  []Source                `toml:"source,omitempty"`
	Packages []Package               `toml:"package,omitempty"`

	raw map[string]any
}

// Defaults are the fallback answers a spec applies when a package does not
// override them.
type Defaults struct {
	Hooks    HooksMode `toml:"hooks,omitempty"`
	Cooldown Duration  `toml:"cooldown,omitempty"`

	raw map[string]any
}

// HostSettings are the per-host switches. Every field is on by default; a
// `false` switches that one behaviour off for that host only (U2).
type HostSettings struct {
	// Enabled false takes a host out of every delivery. It is the coarse
	// switch; the rest are finer.
	Enabled *bool `toml:"enabled,omitempty"`
	// Runtime false keeps a host's JavaScript runtime delivery off while its
	// file delivery stays on (the W3 runtime shim channel).
	Runtime *bool `toml:"runtime,omitempty"`
	// Hooks false never delivers hooks to this host.
	Hooks *bool `toml:"hooks,omitempty"`
}

// Enabled reports whether the host takes part at all; an absent table means yes.
func (h HostSettings) EnabledOrDefault() bool {
	if h.Enabled == nil {
		return true
	}

	return *h.Enabled
}

// RuntimeOn reports whether the host's runtime channel is on; default yes.
func (h HostSettings) RuntimeOn() bool {
	if h.Runtime == nil {
		return true
	}

	return *h.Runtime
}

// HooksOn reports whether hooks may be delivered to the host; default yes.
func (h HostSettings) HooksOn() bool {
	if h.Hooks == nil {
		return true
	}

	return *h.Hooks
}

// HooksMode is the default answer to the hooks consent question.
type HooksMode string

// Hooks consent modes.
const (
	HooksAsk HooksMode = "ask"
	HooksYes HooksMode = "yes"
	HooksNo  HooksMode = "no"
)

// Duration is a TOML string in Go duration syntax ("24h", "1h30m").
type Duration time.Duration

// Duration returns the duration as a standard library value.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// MarshalText encodes the duration in its compact canonical form.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(formatDuration(time.Duration(d))), nil
}

// UnmarshalText parses a duration string and rejects negative values.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", text, err)
	}

	if parsed < 0 {
		return fmt.Errorf("duration %q is negative", text)
	}

	*d = Duration(parsed)

	return nil
}

// Source is a named package source. The declared order is the priority order:
// the first source that resolves an id wins.
type Source struct {
	Name string `toml:"name"`
	URL  string `toml:"url,omitempty"`

	raw map[string]any
}

// Package is one desired package entry.
type Package struct {
	ID          string            `toml:"id"`
	Channel     string            `toml:"channel,omitempty"`
	Except      []string          `toml:"except,omitempty"`
	Env         map[string]string `toml:"env,omitempty"`
	Version     string            `toml:"version,omitempty"` // pin
	Disabled    bool              `toml:"disabled,omitempty"`
	AdoptedFrom string            `toml:"adopted_from,omitempty"`
	Propagate   *Propagate        `toml:"propagate,omitempty"`

	raw map[string]any
}

// New returns an empty schema-1 spec.
func New() *Spec {
	return &Spec{Schema: Schema}
}

// Parse decodes and validates a spec document. A leading UTF-8 BOM is stripped
// before decoding (decision T0.5 Q3): hand-edited TOML from Windows editors
// often carries one and it never changes the document semantics.
func Parse(data []byte) (*Spec, error) {
	return parse(data, "")
}

// ParseFile reads, decodes and validates the spec at path.
func ParseFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller (home/CLI) chooses the path
	if err != nil {
		return nil, fmt.Errorf("read spec %s: %w", path, err)
	}

	return parse(data, path)
}

func parse(data []byte, path string) (*Spec, error) {
	data = stripBOM(data)

	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse spec%s: %w", pathSuffix(path), err)
	}

	if err := checkRawSchema(raw, path); err != nil {
		return nil, err
	}

	if err := checkRawCooldown(raw); err != nil {
		return nil, err
	}

	var spec Spec
	if err := toml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse spec%s: %w", pathSuffix(path), err)
	}

	extractRaw(&spec, raw)

	if err := validate(&spec, path); err != nil {
		return nil, err
	}

	return &spec, nil
}

// Marshal encodes the spec deterministically. Known fields are regenerated
// from the typed spec; unknown fields are merged back at every level, so a
// rewrite never drops them (DESIGN §3.3 round-trip raw).
func (s *Spec) Marshal() ([]byte, error) {
	typed, err := toml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}

	var tree map[string]any
	if err := toml.Unmarshal(typed, &tree); err != nil {
		return nil, fmt.Errorf("decode spec view: %w", err)
	}

	s.mergeRaw(tree)

	return toml.Marshal(tree)
}

// Save writes the spec to path atomically. New files get mode 0600; an
// existing file keeps its mode. The parent directory must exist. A hand-built
// spec with an out-of-range Schema is refused before anything is written:
// Schema > Schema reports *SchemaNewerError, Schema <= 0 *SchemaInvalidError,
// both with Path set.
func (s *Spec) Save(path string) error {
	if err := checkSchema(s.Schema, path); err != nil {
		return err
	}

	data, err := s.Marshal()
	if err != nil {
		return err
	}

	perm := fs.FileMode(0o600)

	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		perm = info.Mode().Perm()
	case !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("stat spec %s: %w", path, statErr)
	}

	if err := fsutil.WriteFileAtomic(path, data, perm); err != nil {
		return fmt.Errorf("save spec %s: %w", path, err)
	}

	return nil
}

// Digest returns the canonical content hash of the spec (DESIGN §6 trust):
// comments, key order and whitespace never change it, semantic edits do.
func (s *Spec) Digest() digest.Hash {
	data, err := s.Marshal()
	if err != nil {
		return ""
	}

	return digest.Bytes(data)
}

// ValidateID reports whether id is a canonical package id per the shared
// grammar in pkg/id: segments of letters, digits, `.`, `_`, `@`, `:`, `+`
// and `-` separated by single `/`, plus at most one `//` separator before a
// non-empty subpath (`owner/repo//skills/foo`, DESIGN §3.4). Empty ids,
// whitespace, control bytes, backslashes, absolute paths, trailing slashes
// and `.`/`..` segments are rejected.
func ValidateID(id string) error {
	if err := pkgid.ValidatePackage(id); err != nil {
		return &InvalidIDError{Value: id, Reason: idReason(err)}
	}

	return nil
}

// idReason extracts the grammar violation reason, falling back to the whole
// error text.
func idReason(err error) string {
	if target, ok := errors.AsType[*pkgid.InvalidIDError](err); ok {
		return target.Reason
	}

	return err.Error()
}

// SchemaNewerError reports a spec whose schema is newer than this version
// supports.

type SchemaNewerError struct {
	Path      string
	Found     int
	Supported int
}

// Error describes the unsupported schema version.
func (e *SchemaNewerError) Error() string {
	return fmt.Sprintf("spec schema %d is newer than supported %d%s", e.Found, e.Supported, pathSuffix(e.Path))
}

// SchemaInvalidError reports a missing or non-positive schema version.

type SchemaInvalidError struct {
	Path  string
	Found int
}

// Error describes the invalid schema version.
func (e *SchemaInvalidError) Error() string {
	return fmt.Sprintf("spec schema %d is invalid%s", e.Found, pathSuffix(e.Path))
}

// DuplicateIDError reports a repeated package id or source name.

type DuplicateIDError struct {
	Kind string
	ID   string
}

// Error describes the duplicated id.
func (e *DuplicateIDError) Error() string {
	return fmt.Sprintf("duplicate %s id %q", e.Kind, e.ID)
}

// InvalidValueError reports a schema value that failed validation.

type InvalidValueError struct {
	Table  string
	Key    string
	Value  string
	Reason string
}

// Error describes the rejected value.
func (e *InvalidValueError) Error() string {
	return fmt.Sprintf("invalid %s.%s value %q: %s", e.Table, e.Key, e.Value, e.Reason)
}

// InvalidIDError reports an id that is unsafe as a path.

type InvalidIDError struct {
	Value  string
	Reason string
}

// Error describes the rejected id.
func (e *InvalidIDError) Error() string {
	return fmt.Sprintf("invalid id %q: %s", e.Value, e.Reason)
}

func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}

func pathSuffix(path string) string {
	if path == "" {
		return ""
	}

	return " (" + path + ")"
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	if d < 0 {
		return d.String()
	}

	var b strings.Builder

	if hours := d / time.Hour; hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}

	if minutes := (d % time.Hour) / time.Minute; minutes > 0 {
		fmt.Fprintf(&b, "%dm", minutes)
	}

	if seconds := (d % time.Minute) / time.Second; seconds > 0 {
		fmt.Fprintf(&b, "%ds", seconds)
	}

	if rest := d % time.Second; rest > 0 {
		b.WriteString(rest.String())
	}

	return b.String()
}

// checkRawCooldown validates the raw cooldown value before the typed decode, so
// a numeric or malformed cooldown yields *InvalidValueError instead of a
// toml.DecodeError (the taxonomy TestParseErrors pins). It runs after
// checkRawSchema, so a newer or absent schema never reaches it.
func checkRawCooldown(raw map[string]any) error {
	defaults, ok := raw[defaultsTable].(map[string]any)
	if !ok {
		return nil
	}

	value, ok := defaults[cooldownKey]
	if !ok {
		return nil
	}

	text, ok := value.(string)
	if !ok {
		return &InvalidValueError{
			Table: defaultsTable, Key: cooldownKey,
			Value:  fmt.Sprintf("%v", value),
			Reason: "expected a duration string",
		}
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return &InvalidValueError{Table: defaultsTable, Key: cooldownKey, Value: text, Reason: "not a duration"}
	}

	if parsed < 0 {
		return &InvalidValueError{Table: defaultsTable, Key: cooldownKey, Value: text, Reason: "negative duration"}
	}

	return nil
}

// checkRawSchema applies the schema guard to the raw document before any value
// validation, so a newer (or absent) schema is refused with *SchemaNewerError /
// *SchemaInvalidError even when the document also carries a future
// representation of a validated value, e.g. a numeric cooldown (review T0.5
// N1). A present but non-integer schema falls through to the typed decode,
// which reports the TOML type error.
func checkRawSchema(raw map[string]any, path string) error {
	value, present := raw["schema"]
	if !present {
		return &SchemaInvalidError{Path: path}
	}

	schema, ok := value.(int64)
	if !ok {
		return nil
	}

	return checkSchema(int(schema), path)
}

// checkSchema is the single schema guard shared by raw parsing and typed
// validation.
func checkSchema(schema int, path string) error {
	switch {
	case schema > Schema:
		return &SchemaNewerError{Path: path, Found: schema, Supported: Schema}
	case schema <= 0:
		return &SchemaInvalidError{Path: path, Found: schema}
	}

	return nil
}

func validate(spec *Spec, path string) error {
	if err := checkSchema(spec.Schema, path); err != nil {
		return err
	}

	if hooks := spec.Defaults.Hooks; hooks != "" && !hooks.valid() {
		return &InvalidValueError{
			Table: defaultsTable, Key: "hooks",
			Value:  string(hooks),
			Reason: "expected ask, yes or no",
		}
	}

	if err := validateSources(spec.Sources); err != nil {
		return err
	}

	if err := validatePropagate(&spec.Propagate, "propagate"); err != nil {
		return err
	}

	return validatePackages(spec.Packages)
}

func validateSources(sources []Source) error {
	seen := make(map[string]struct{}, len(sources))

	for _, src := range sources {
		if src.Name == "" {
			return &InvalidValueError{Table: "source", Key: "name", Value: "", Reason: "empty source name"}
		}

		if _, duplicate := seen[src.Name]; duplicate {
			return &DuplicateIDError{Kind: "source", ID: src.Name}
		}

		seen[src.Name] = struct{}{}
	}

	return nil
}

func validatePackages(packages []Package) error {
	seen := make(map[string]struct{}, len(packages))

	for _, pkg := range packages {
		if err := ValidateID(pkg.ID); err != nil {
			// Parse reports an unsafe id through InvalidValueError only, without
			// an InvalidIDError chain; ValidateID itself returns InvalidIDError
			// (decision T0.5 Q4).
			return &InvalidValueError{Table: packageTable, Key: "id", Value: pkg.ID, Reason: err.Error()}
		}

		if _, duplicate := seen[pkg.ID]; duplicate {
			return &DuplicateIDError{Kind: "package", ID: pkg.ID}
		}

		seen[pkg.ID] = struct{}{}

		if slices.Contains(pkg.Except, "") {
			return &InvalidValueError{
				Table: packageTable, Key: "except",
				Value:  "",
				Reason: "empty except element",
			}
		}

		if err := validatePropagate(pkg.Propagate, "package.propagate"); err != nil {
			return err
		}
	}

	return nil
}

func validatePropagate(policy *Propagate, table string) error {
	if policy == nil {
		return nil
	}

	for _, ev := range events {
		value := policy.Mode(ev)
		if value != "" && !value.valid() {
			return &InvalidValueError{
				Table: table, Key: string(ev),
				Value:  string(value),
				Reason: "expected all, origin or ask",
			}
		}
	}

	for _, name := range slices.Sorted(maps.Keys(policy.Kind)) {
		sub := policy.Kind[name]
		if err := validatePropagate(&sub, table+".kind."+name); err != nil {
			return err
		}
	}

	for _, name := range slices.Sorted(maps.Keys(policy.Host)) {
		sub := policy.Host[name]
		if err := validatePropagate(&sub, table+".host."+name); err != nil {
			return err
		}
	}

	return nil
}

// extractRaw stashes the unknown fields of every known table into its raw
// map, so Marshal can put them back.
func extractRaw(spec *Spec, raw map[string]any) {
	spec.raw = unknownKeys(raw, topKeys[:])

	if defaults, ok := raw[defaultsTable].(map[string]any); ok {
		spec.Defaults.raw = unknownKeys(defaults, defaultsKeys[:])
	}

	if propagate, ok := raw["propagate"].(map[string]any); ok {
		extractPropagateRaw(&spec.Propagate, propagate)
	}

	if sources, ok := raw["source"].([]any); ok {
		for i := range spec.Sources {
			if i >= len(sources) {
				break
			}

			element, ok := sources[i].(map[string]any)
			if !ok {
				continue
			}

			spec.Sources[i].raw = unknownKeys(element, sourceKeys[:])
		}
	}

	if packages, ok := raw[packageTable].([]any); ok {
		for i := range spec.Packages {
			if i >= len(packages) {
				break
			}

			element, ok := packages[i].(map[string]any)
			if !ok {
				continue
			}

			extractPackageRaw(&spec.Packages[i], element)
		}
	}
}

func extractPropagateRaw(policy *Propagate, table map[string]any) {
	policy.raw = unknownKeys(table, propagateKeys[:])

	if kind, ok := table["kind"].(map[string]any); ok {
		policy.Kind = extractPolicyRaw(policy.Kind, kind)
	}

	if host, ok := table["host"].(map[string]any); ok {
		policy.Host = extractPolicyRaw(policy.Host, host)
	}
}

func extractPolicyRaw(policies map[string]Propagate, table map[string]any) map[string]Propagate {
	if policies == nil {
		policies = make(map[string]Propagate, len(table))
	}

	for name, value := range table {
		rawTable, ok := value.(map[string]any)
		if !ok {
			continue
		}

		policy := policies[name]
		policy.raw = unknownKeys(rawTable, eventKeys[:])
		policies[name] = policy
	}

	return policies
}

func extractPackageRaw(pkg *Package, element map[string]any) {
	pkg.raw = unknownKeys(element, packageKeys[:])

	propagate, ok := element["propagate"].(map[string]any)
	if !ok {
		return
	}

	if pkg.Propagate == nil {
		pkg.Propagate = &Propagate{}
	}

	extractPropagateRaw(pkg.Propagate, propagate)
}

// mergeRaw puts the unknown fields of the parsed document back into the
// marshalled typed view.
func (s *Spec) mergeRaw(tree map[string]any) {
	mergeUnknown(tree, s.raw)

	if len(s.Defaults.raw) > 0 {
		mergeUnknown(nestedTable(tree, defaultsTable), s.Defaults.raw)
	}

	if s.Propagate.hasRaw() {
		s.Propagate.mergeRaw(nestedTable(tree, "propagate"))
	}

	for i := range s.Sources {
		element := findElement(tree, "source", "name", s.Sources[i].Name)
		if element == nil {
			continue
		}

		mergeUnknown(element, s.Sources[i].raw)
	}

	for i := range s.Packages {
		pkg := &s.Packages[i]

		element := findElement(tree, packageTable, "id", pkg.ID)
		if element == nil {
			continue
		}

		mergeUnknown(element, pkg.raw)

		if pkg.Propagate != nil && pkg.Propagate.hasRaw() {
			pkg.Propagate.mergeRaw(nestedTable(element, "propagate"))
		}
	}
}

// mergeRaw puts the unknown fields of one policy (and its nested overrides)
// back into the marshalled tree.
func (p *Propagate) mergeRaw(table map[string]any) {
	if p == nil {
		return
	}

	mergeUnknown(table, p.raw)

	for name, policy := range p.Kind {
		if !policy.hasRaw() {
			continue
		}

		policy.mergeRaw(nestedTable(nestedTable(table, "kind"), name))
	}

	for name, policy := range p.Host {
		if !policy.hasRaw() {
			continue
		}

		policy.mergeRaw(nestedTable(nestedTable(table, "host"), name))
	}
}

func (p *Propagate) hasRaw() bool {
	if p == nil {
		return false
	}

	if len(p.raw) > 0 {
		return true
	}

	for _, policy := range p.Kind {
		if policy.hasRaw() {
			return true
		}
	}

	for _, policy := range p.Host {
		if policy.hasRaw() {
			return true
		}
	}

	return false
}

func unknownKeys(table map[string]any, known []string) map[string]any {
	var out map[string]any

	for key, value := range table {
		if slices.Contains(known, key) {
			continue
		}

		if out == nil {
			out = make(map[string]any)
		}

		out[key] = value
	}

	return out
}

func mergeUnknown(dst, extra map[string]any) {
	for key, value := range extra {
		existing, ok := dst[key]
		if !ok {
			dst[key] = value

			continue
		}

		existingTable, okExisting := existing.(map[string]any)
		valueTable, okValue := value.(map[string]any)

		if okExisting && okValue {
			mergeUnknown(existingTable, valueTable)

			continue
		}

		dst[key] = value
	}
}

func nestedTable(parent map[string]any, key string) map[string]any {
	table, ok := parent[key].(map[string]any)
	if !ok {
		table = make(map[string]any)
		parent[key] = table
	}

	return table
}

func findElement(tree map[string]any, arrayKey, idKey, value string) map[string]any {
	elements, ok := tree[arrayKey].([]any)
	if !ok {
		return nil
	}

	for _, element := range elements {
		table, ok := element.(map[string]any)
		if !ok {
			continue
		}

		name, ok := table[idKey].(string)
		if ok && name == value {
			return table
		}
	}

	return nil
}
