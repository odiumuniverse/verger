package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A channel is a name a user writes instead of a version: `stable`, `beta`,
// `latest`, or a branch. Resolving one is deciding `Ref.Rev` - the tag, branch
// or commit the fetch checks out - so this is a resolver for names, not a
// second fetch path.
//
// The three decisions the product fixed, and where they come from:
//
//   - npm: the channel is a dist-tag, and `latest` is what an empty channel
//     means. npm has no other notion of "track the newest of this line".
//   - git: `stable` is the highest semver tag that is not a prerelease;
//     `beta`, `next`, or any other prerelease name is the highest tag carrying
//     that prerelease; anything else is a branch, resolved to its tip.
//   - archive and local have no channel. There is nothing to track, so saying
//     so is the honest answer and a UsageError upstream, not a silent ignore.
//
// A channel that names nothing is an error listing what does exist. Falling
// back to the newest of something would be a different product, chosen by the
// code instead of by the user.

// ChannelResolution is where one channel pointed.
type ChannelResolution struct {
	// Channel is the name that was asked for, as written.
	Channel string
	// Rev is what the fetch should check out: a tag, a branch, or a dist-tag
	// target resolved to a version.
	Rev string
	// Version is the version Rev names, empty when Rev is a branch.
	Version string
	// Via says how it was found - "tag", "branch" or "dist-tag" - so a report
	// can say where a rev came from instead of only that it changed.
	Via string
}

// ChannelError is a channel that could not be resolved. It carries what the
// source does offer, because "no such channel" without the list is a question
// the user can only answer by guessing.
type ChannelError struct {
	// Ref is the ref the channel was asked of.
	Ref string
	// Channel is the name that did not resolve.
	Channel string
	// Available is every channel this source does offer, sorted.
	Available []string
	// Reason is the sentence for the user. It is set only where the refusal is
	// about the ref rather than about a missing name.
	Reason string
}

// Error implements error.
func (e *ChannelError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("channel on %s: %s", e.Ref, e.Reason)
	}

	return fmt.Sprintf("channel %q not found on %s (available: %s)",
		e.Channel, e.Ref, strings.Join(e.Available, ", "))
}

// NotApplicable is the reason a channel cannot apply to this kind of ref.
const NotApplicable = "channel applies to npm and git sources only"

// ChannelApplies reports whether a channel means anything for this kind of ref.
func ChannelApplies(k Kind) bool {
	switch k {
	case KindNPM, KindGit, KindGitHub:
		return true
	case KindLocal, KindURL, KindMCP, KindAgent:
		return false
	default:
		return false
	}
}

// ResolveChannel turns a channel name into something the fetch can check out.
func (f *Fetcher) ResolveChannel(ctx context.Context, ref Ref, channel string) (ChannelResolution, error) {
	if !ChannelApplies(ref.Kind) {
		return ChannelResolution{}, &ChannelError{Ref: ref.Raw, Channel: channel, Reason: NotApplicable}
	}

	if ref.Kind == KindNPM {
		return f.resolveDistTag(ctx, ref, channel)
	}

	return f.resolveGitChannel(ctx, ref, channel)
}

// resolveGitChannel answers a channel from a git remote: `ls-remote` for the
// tags and the heads, and the answer is whichever of them the name belongs to.
func (f *Fetcher) resolveGitChannel(ctx context.Context, ref Ref, channel string) (ChannelResolution, error) {
	git, err := f.tool("git")
	if err != nil {
		return ChannelResolution{}, err
	}

	if ref.URL == "" {
		return ChannelResolution{}, &ChannelError{
			Ref: ref.Raw, Channel: channel, Reason: "git refs need a url",
		}
	}

	tagsOut, tagsErr := git.RunWith(ctx, f.runner, []string{"ls-remote", "--tags", ref.URL}, nil)
	headsOut, headsErr := git.RunWith(ctx, f.runner, []string{"ls-remote", "--heads", ref.URL}, nil)

	if tagsErr != nil && headsErr != nil {
		return ChannelResolution{}, tagsErr
	}

	tags := remoteRefs(string(tagsOut), "refs/tags/")
	heads := remoteRefs(string(headsOut), "refs/heads/")

	// A branch of the same name is what the user meant unless the name is one
	// of the channel words: `stable` and `beta` are words verger gives a
	// meaning, and a repo that happens to have a `stable` branch does not get
	// to redefine what the word means in a spec.
	if isVersionChannel(channel) {
		if best, ok := pickSemverTag(tags, channel); ok {
			return ChannelResolution{Channel: channel, Rev: best.name, Version: best.string(), Via: "tag"}, nil
		}

		return ChannelResolution{}, &ChannelError{
			Ref: ref.Raw, Channel: channel, Available: channelNames(tags, heads),
		}
	}

	if sha, ok := heads[channel]; ok {
		return ChannelResolution{Channel: channel, Rev: channel, Version: sha, Via: "branch"}, nil
	}

	if best, ok := pickSemverTag(tags, channel); ok {
		return ChannelResolution{Channel: channel, Rev: best.name, Version: best.string(), Via: "tag"}, nil
	}

	return ChannelResolution{}, &ChannelError{
		Ref: ref.Raw, Channel: channel, Available: channelNames(tags, heads),
	}
}

// resolveDistTag answers a channel from an npm registry. The channel is the
// dist-tag name; an empty channel is `latest`, which is what npm itself means
// by the newest published version.
func (f *Fetcher) resolveDistTag(ctx context.Context, ref Ref, channel string) (ChannelResolution, error) {
	name := ref.NPM
	if at := strings.LastIndex(name, "@"); at > 0 {
		name = name[:at]
	}

	if name == "" {
		return ChannelResolution{}, &ChannelError{Ref: ref.Raw, Channel: channel, Reason: "npm refs need a name"}
	}

	if channel == "" {
		channel = "latest"
	}

	tags, err := f.distTags(ctx, name)
	if err != nil {
		return ChannelResolution{}, err
	}

	version, ok := tags[channel]
	if !ok {
		available := make([]string, 0, len(tags))
		for tag := range tags {
			available = append(available, tag)
		}

		slices.Sort(available)

		return ChannelResolution{}, &ChannelError{
			Ref: ref.Raw, Channel: channel, Available: available,
		}
	}

	return ChannelResolution{Channel: channel, Rev: name + "@" + version, Version: version, Via: "dist-tag"}, nil
}

// distTags reads one package's dist-tags from the registry.
func (f *Fetcher) distTags(ctx context.Context, name string) (map[string]string, error) {
	endpoint := strings.TrimSuffix(f.registry, "/") + "/" + name

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("npm registry %s: %w", name, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &ChannelError{Ref: name, Reason: "registry returned " + resp.Status}
	}

	var document struct {
		DistTags map[string]string `json:"dist-tags"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return nil, fmt.Errorf("npm registry %s: %w", name, err)
	}

	if len(document.DistTags) == 0 {
		return nil, &ChannelError{Ref: name, Reason: "the registry listed no dist-tags"}
	}

	return document.DistTags, nil
}

// remoteRefs parses `ls-remote` output into name -> sha, dropping the
// `^{}` entries git adds for annotated tags: they point at the same tag and
// would otherwise make every tag look like it had a duplicate.
func remoteRefs(out, prefix string) map[string]string {
	found := map[string]string{}

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !strings.HasPrefix(fields[1], prefix) {
			continue
		}

		name := strings.TrimPrefix(fields[1], prefix)
		if strings.HasSuffix(name, "^{}") {
			continue
		}

		found[name] = fields[0]
	}

	return found
}

// channelNames is what a user may write: `stable` and `beta` are always
// offered, plus every prerelease word the tags carry and every branch.
func channelNames(tags, heads map[string]string) []string {
	names := []string{"stable", "beta"}

	for name := range tags {
		if _, version, ok := parseSemver(name); ok && version.pre != "" {
			names = append(names, version.pre)
		}
	}

	for name := range heads {
		names = append(names, name)
	}

	slices.Sort(names)

	return slices.Compact(names)
}

// pickedTag is one tag that answered a channel.
type pickedTag struct {
	name    string
	version semver
}

// string renders the version the way it was written, without the leading v a
// tag may carry: this is the value a manifest records, not the tag name.
func (t pickedTag) string() string {
	if t.version.pre != "" {
		return t.version.core + "-" + t.version.pre
	}

	return t.version.core
}

// pickSemverTag finds the highest tag for a channel: `stable` takes the
// highest release, and a prerelease word takes the highest tag carrying it.
// Among candidates the highest version wins, and a tie goes to the larger build
// number so a re-tagged patch release is not silently the older one.
func pickSemverTag(tags map[string]string, channel string) (pickedTag, bool) {
	best, found := pickedTag{}, false

	for name := range tags {
		_, version, ok := parseSemver(name)
		if !ok {
			continue
		}

		if !channelMatches(version, channel) {
			continue
		}

		if !found || compareSemver(version, best.version) > 0 {
			best, found = pickedTag{name: name, version: version}, true
		}
	}

	return best, found
}

// isVersionChannel reports whether a name is one of the words verger gives a
// meaning, rather than a branch or tag the remote happens to have.
func isVersionChannel(name string) bool {
	return name == "stable" || name != "" && strings.ContainsAny(name, "-")
}

// channelMatches reports whether a parsed version belongs to a channel.
func channelMatches(version semver, channel string) bool {
	switch channel {
	case "stable":
		return version.pre == ""
	case "":
		return false
	default:
		return version.pre != "" && strings.HasPrefix(version.pre, channel)
	}
}

// semver is the subset of a version string a channel decision needs: the three
// numbers, the prerelease identifier and the build. It is not a validator - a
// tag that is not semver simply is not a candidate, and parseSemver says so.
type semver struct {
	major, minor, patch int
	pre                 string
	build               int
	core                string
}

// parseSemver reads a leading `v`-prefixed semver out of a tag name.
func parseSemver(name string) (string, semver, bool) {
	body := strings.TrimPrefix(name, "v")
	if body == "" {
		return "", semver{}, false
	}

	rest, build := body, 0
	if _, buildMeta, ok := strings.Cut(body, "+"); ok {
		n, err := strconv.Atoi(buildMeta)
		if err != nil {
			return "", semver{}, false
		}

		rest, build = strings.TrimSuffix(body, "+"+buildMeta), n
	}

	pre := ""
	if at := strings.IndexByte(rest, '-'); at >= 0 {
		pre, rest = rest[at+1:], rest[:at]
	}

	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return "", semver{}, false
	}

	numbers := make([]int, 3)

	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return "", semver{}, false
		}

		numbers[i] = n
	}

	return name, semver{
		major: numbers[0], minor: numbers[1], patch: numbers[2],
		pre: pre, build: build, core: rest,
	}, true
}

// compareSemver orders two versions by precedence, which puts a prerelease
// below its own release: 1.2.0-beta is older than 1.2.0, which is the whole
// reason `stable` and `beta` do not name the same tag.
func compareSemver(a, b semver) int {
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] != pair[1] {
			return compareInts(pair[0], pair[1])
		}
	}

	switch {
	case a.pre == b.pre:
		return compareInts(a.build, b.build)
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	case a.pre < b.pre:
		return -1
	default:
		return 1
	}
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// ErrNoRegistry is returned when a dist-tag is asked of a fetcher with no
// registry to ask; it exists so the message names the cause rather than
// reporting an empty channel list.
var ErrNoRegistry = errors.New("no npm registry configured")

// client is the http client a registry read uses, defaulted once.
func (f *Fetcher) client() *http.Client {
	if f.httpClient != nil {
		return f.httpClient
	}

	return &http.Client{Timeout: 30 * time.Second}
}

// CompareVersions orders two version strings by semver precedence and returns
// -1, 0 or 1. It exists because "is this an upgrade" is a question the facade
// has to answer, and the semver machinery already here is the answer - a second
// comparator beside it would eventually disagree with the first about which of
// two versions is newer, which is the one bug this ordering is for.
//
// A string that is not a version at all compares as 0: an unparseable version is
// not evidence of a downgrade, and refusing to compare would make a package
// whose version nobody formats un-updatable.
func CompareVersions(a, b string) int {
	_, av, aok := parseSemver(a)

	_, bv, bok := parseSemver(b)
	if !aok || !bok {
		return 0
	}

	return compareSemver(av, bv)
}
