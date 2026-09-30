package runtime

import (
	"encoding/json"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/manifest"
)

// A nil slice serializes as JSON `null`, and the bundle iterates both lists
// unconditionally. A manifest written that way did not fail loudly in Go: the
// module loaded, the runtime wrote its heartbeat, and the hooks never ran —
// the runtime logged `locked: scripts is not iterable` and the delivery still
// read as successful. A live test against pi caught it; this pins the shape so
// it cannot come back.
func TestManifestSerializesEmptyListsAsArrays(t *testing.T) {
	Convey("Given a package with hooks and no script files", t, func() {
		hooks := []manifest.Hook{{Event: "SessionStart", Matcher: "", Command: "true", Timeout: 5}}
		m := NewManifest("acme/x", "1.0.0", hooks, nil)

		data, err := json.Marshal(m)
		So(err, ShouldBeNil)

		var doc map[string]json.RawMessage
		So(json.Unmarshal(data, &doc), ShouldBeNil)

		Convey("Then both lists are JSON arrays, never null", func() {
			So(string(doc["scripts"]), ShouldEqual, `[]`)
			So(string(doc["hooks"]), ShouldNotEqual, `null`)
		})

		Convey("And a package with no hooks at all still carries arrays", func() {
			empty, err := json.Marshal(NewManifest("acme/y", "1.0.0", nil, nil))
			So(err, ShouldBeNil)

			var emptyDoc map[string]json.RawMessage
			So(json.Unmarshal(empty, &emptyDoc), ShouldBeNil)
			So(string(emptyDoc["hooks"]), ShouldEqual, `[]`)
			So(string(emptyDoc["scripts"]), ShouldEqual, `[]`)
		})
	})
}
