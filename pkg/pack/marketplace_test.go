package pack

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// TestRenderMarketplace pins the owner marketplace document (decision F3): a
// pure function of the marketplace name and its entries, sorted by name,
// every source a dir inside the marketplace root.
func TestRenderMarketplace(t *testing.T) {
	Convey("Given two entries of one owner out of order", t, func() {
		entries := []MarketplaceEntry{
			{Name: "foo", Source: "./foo/1.1.0"},
			{Name: "bar", Source: "./bar/2.0.0"},
		}

		Convey("When the marketplace is rendered", func() {
			data, err := RenderMarketplace("acme", entries)

			Convey("Then it is the exact owner document, entries sorted", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldEqualJSON, `{
  "name": "acme",
  "owner": {"name": "acme"},
  "plugins": [
    {"name": "bar", "source": "./bar/2.0.0"},
    {"name": "foo", "source": "./foo/1.1.0"}
  ]
}`)
			})

			Convey("Then a second render is byte-identical and the input is untouched", func() {
				again, againErr := RenderMarketplace("acme", entries)
				So(againErr, ShouldBeNil)
				So(again, ShouldResemble, data)
				So(entries[0].Name, ShouldEqual, "foo")
			})
		})
	})

	Convey("Given an owner without entries", t, func() {
		data, err := RenderMarketplace("acme", nil)

		Convey("When it is rendered", func() {
			Convey("Then the plugins list is empty, not null", func() {
				So(err, ShouldBeNil)
				So(string(data), ShouldEqualJSON, `{"name": "acme", "owner": {"name": "acme"}, "plugins": []}`)
			})
		})
	})

	Convey("Given invalid marketplace inputs", t, func() {
		cases := map[string]struct {
			name    string
			entries []MarketplaceEntry
		}{
			"a name with @":        {"acme@x", nil},
			"an empty name":        {"", nil},
			"a plugin with @":      {"acme", []MarketplaceEntry{{Name: "foo@acme", Source: "./foo/1.0.0"}}},
			"a duplicate plugin":   {"acme", []MarketplaceEntry{{Name: "foo", Source: "./foo/1.0.0"}, {Name: "foo", Source: "./foo/2.0.0"}}},
			"a source outside":     {"acme", []MarketplaceEntry{{Name: "foo", Source: "../foo/1.0.0"}}},
			"an absolute source":   {"acme", []MarketplaceEntry{{Name: "foo", Source: "/tmp/foo"}}},
			"the root as source":   {"acme", []MarketplaceEntry{{Name: "foo", Source: "./"}}},
			"an undotted source":   {"acme", []MarketplaceEntry{{Name: "foo", Source: "foo/1.0.0"}}},
			"a climbing source":    {"acme", []MarketplaceEntry{{Name: "foo", Source: "./foo/../../x"}}},
			"an unclean source":    {"acme", []MarketplaceEntry{{Name: "foo", Source: "./foo//1.0.0"}}},
			"a source with a NUL":  {"acme", []MarketplaceEntry{{Name: "foo", Source: "./foo/1\x00"}}},
			"a plugin with a dash": {"acme", []MarketplaceEntry{{Name: "-foo", Source: "./foo/1.0.0"}}},
		}

		for label, tc := range cases {
			Convey("When the marketplace has "+label, func() {
				_, err := RenderMarketplace(tc.name, tc.entries)

				Convey("Then it is refused with a *RenderError", func() {
					_ = renderError(t, err)
				})
			})
		}
	})
}

// TestParseMarketplace pins the read-back of an owner marketplace: exactly what
// RenderMarketplace writes, anything else refused.
func TestParseMarketplace(t *testing.T) {
	Convey("Given a rendered owner marketplace", t, func() {
		data, err := RenderMarketplace("acme", []MarketplaceEntry{
			{Name: "foo", Source: "./foo/1.1.0"},
			{Name: "bar", Source: "./bar/2.0.0"},
		})
		So(err, ShouldBeNil)

		Convey("When it is parsed", func() {
			name, entries, parseErr := ParseMarketplace(data)

			Convey("Then the name and the sorted entries come back", func() {
				So(parseErr, ShouldBeNil)
				So(name, ShouldEqual, "acme")
				So(entries, ShouldResemble, []MarketplaceEntry{
					{Name: "bar", Source: "./bar/2.0.0"},
					{Name: "foo", Source: "./foo/1.1.0"},
				})
			})
		})
	})

	Convey("Given documents that are not an owner marketplace", t, func() {
		for _, doc := range []string{
			"not json",
			`{"plugins": []}`,
			`{"name": "acme", "plugins": {}}`,
			`{"name": "acme", "plugins": [{"name": "foo"}]}`,
			`{"name": "acme", "plugins": [{"name": "foo", "source": "../escape"}]}`,
			`{"name": "acme", "plugins": [{"name": "foo", "source": {"source": "github"}}]}`,
		} {
			Convey("When "+doc+" is parsed", func() {
				_, _, err := ParseMarketplace([]byte(doc))

				Convey("Then it is refused with a *RenderError", func() {
					_ = renderError(t, err)
				})
			})
		}
	})
}
