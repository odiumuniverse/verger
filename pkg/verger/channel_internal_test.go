package verger

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// Given a spec that names channels
// When the refs are matched against it
// Then the channel lands on the ref that names the same package.
func TestApplyChannelsPutsTheChannelOnTheRef(t *testing.T) {
	Convey("Given a spec whose packages name channels", t, func() {
		sp := spec.New()
		sp.Packages = []spec.Package{
			{ID: "caveman", Channel: "stable"},
			{ID: "other", Channel: "beta"},
			{ID: "pinned", Version: "1.2.3"},
			{ID: "silent"},
		}

		Convey("When refs are matched against it", func() {
			refs := []source.Ref{
				{Kind: source.KindGitHub, ID: "caveman", Raw: "caveman"},
				{Kind: source.KindNPM, ID: "other", Raw: "other"},
				{Kind: source.KindGitHub, ID: "pinned", Raw: "pinned@1.2.3"},
				{Kind: source.KindNPM, ID: "silent", Raw: "silent"},
			}

			Convey("Then each ref carries only the channel its own entry names", func() {
				applyChannels(sp, refs)

				So(refs[0].Channel, ShouldEqual, "stable")
				So(refs[1].Channel, ShouldEqual, "beta")
				So(refs[2].Channel, ShouldEqual, "")
				So(refs[3].Channel, ShouldEqual, "")
			})
		})

		Convey("When a ref names a package the spec does not declare", func() {
			refs := []source.Ref{{Kind: source.KindNPM, ID: "stranger", Raw: "stranger"}}

			Convey("Then it keeps an empty channel and is fetched as written", func() {
				applyChannels(sp, refs)

				So(refs[0].Channel, ShouldEqual, "")
			})
		})

		Convey("When a ref names a version of a package the spec does declare", func() {
			refs := []source.Ref{{Kind: source.KindNPM, ID: "caveman", Raw: "caveman@1.2.3"}}

			Convey("Then the channel still applies, because it is the same entry", func() {
				// Pinning a version and naming a channel are two statements
				// about one package. Refusing to match here would make the
				// channel a line the reader believes is in force.
				applyChannels(sp, refs)

				So(refs[0].Channel, ShouldEqual, "stable")
			})
		})

		Convey("When the spec is nil", func() {
			refs := []source.Ref{{Kind: source.KindNPM, ID: "caveman", Raw: "caveman"}}

			Convey("Then the refs are left alone rather than dereferenced", func() {
				applyChannels(nil, refs)

				So(refs[0].Channel, ShouldEqual, "")
			})
		})
	})
}

// Given a spec entry
// When its channel is asked for
// Then it is reported, and a package the spec does not name has none.
func TestChannelOfReportsWhatTheEntryNames(t *testing.T) {
	Convey("Given a spec with one channel and one plain entry", t, func() {
		sp := spec.New()
		sp.Packages = []spec.Package{
			{ID: "caveman", Channel: "stable"},
			{ID: "plain"},
		}

		Convey("When the channel of a known id is asked for", func() {
			Convey("Then it is the one that entry names", func() {
				So(channelOf(sp, "caveman"), ShouldEqual, "stable")
			})
		})

		Convey("When the channel of an entry with no channel is asked for", func() {
			Convey("Then it is empty rather than a guess", func() {
				So(channelOf(sp, "plain"), ShouldEqual, "")
			})
		})

		Convey("When the channel of a package the spec does not name is asked for", func() {
			Convey("Then it is empty", func() {
				So(channelOf(sp, "stranger"), ShouldEqual, "")
			})
		})

		Convey("When a disabled entry is asked for", func() {
			sp.Packages[0].Disabled = true

			Convey("Then its channel is still reported", func() {
				// Whether it installs this run is a different question from which
				// version it would install as; a disabled entry that silently
				// lost its channel would report the wrong answer to the second.
				So(channelOf(sp, "caveman"), ShouldEqual, "stable")
			})
		})
	})
}
