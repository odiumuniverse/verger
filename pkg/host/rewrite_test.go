package host

import (
	"errors"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestRewriteVariablesKnown(t *testing.T) {
	const (
		root = "/store/data/acme/caveman"
		data = "/store/data/acme/caveman/claude"
	)

	Convey("Given values carrying plugin root and data variables", t, func() {
		cases := []struct {
			name string
			in   string
			want string
		}{
			{"claude root", `node "${CLAUDE_PLUGIN_ROOT}/x.js"`, `node "` + root + `/x.js"`},
			{"portable root", `node ${PLUGIN_ROOT}/x.js`, `node ` + root + `/x.js`},
			{"claude data", `${CLAUDE_PLUGIN_DATA}/cache`, data + `/cache`},
			{"portable data", `${PLUGIN_DATA}/cache`, data + `/cache`},
			{"two roots", `${PLUGIN_ROOT}/a:${CLAUDE_PLUGIN_ROOT}/b`, root + `/a:` + root + `/b`},
			{"no variables", `node ./server.js --flag`, `node ./server.js --flag`},
		}

		for _, item := range cases {
			Convey("When the "+item.name+" value is rewritten", func() {
				got, err := rewriteVariables(item.in, root, data, nil)

				Convey("Then the known variables become absolute paths", func() {
					So(err, ShouldBeNil)
					So(got, ShouldEqual, item.want)
				})
			})
		}
	})

	Convey("Given ordinary shell and template text", t, func() {
		cases := []string{`echo $HOME`, `echo $1`, `{{args}}`, `{env:MY_VAR}`, `$`}

		for _, value := range cases {
			Convey("When "+value+" is rewritten", func() {
				got, err := rewriteVariables(value, root, data, nil)

				Convey("Then it stays verbatim", func() {
					So(err, ShouldBeNil)
					So(got, ShouldEqual, value)
				})
			})
		}
	})
}

func TestRewriteVariablesUnsupported(t *testing.T) {
	Convey("Given values carrying an unresolvable host variable", t, func() {
		cases := []struct {
			name     string
			in       string
			variable string
		}{
			{"unknown braced", `run ${FOO_BAR}`, "FOO_BAR"},
			{"project dir", `cd ${CLAUDE_PROJECT_DIR}`, "CLAUDE_PROJECT_DIR"},
			{"gemini extension path", `${extensionPath}/x`, "extensionPath"},
			{"cursor root", `${CURSOR_PLUGIN_ROOT}/x`, "CURSOR_PLUGIN_ROOT"},
			{"workspace path", `${workspacePath}/x`, "workspacePath"},
			{"unbraced root", `run $PLUGIN_ROOT/x`, "$PLUGIN_ROOT"},
			{"unbraced claude data", `run $CLAUDE_PLUGIN_DATA/x`, "$CLAUDE_PLUGIN_DATA"},
			{"nested", `${A${B}}`, "A${B"},
			{"unterminated", `run ${`, "${"},
		}

		for _, item := range cases {
			Convey("When the "+item.name+" value is rewritten", func() {
				_, err := rewriteVariables(item.in, "/root", "/data", nil)

				typed, ok := errors.AsType[*UnsupportedVariableError](err)

				Convey("Then rewrite refuses with the variable named", func() {
					So(ok, ShouldBeTrue)
					So(typed.Variable, ShouldEqual, item.variable)
				})
			})
		}
	})

	Convey("Given the Gemini dialect variables", t, func() {
		extra := map[string]string{"extensionPath": "/store/synth/acme/caveman"}

		Convey("When extensionPath is rewritten with the dialect set", func() {
			got, err := rewriteVariables(`${extensionPath}/x`, "/root", "/data", extra)

			Convey("Then it becomes the extension root", func() {
				So(err, ShouldBeNil)
				So(got, ShouldEqual, "/store/synth/acme/caveman/x")
			})
		})

		Convey("When another dialect's variable is rewritten", func() {
			_, err := rewriteVariables(`${CURSOR_PLUGIN_ROOT}/x`, "/root", "/data", extra)

			Convey("Then it is still refused", func() {
				_, ok := errors.AsType[*UnsupportedVariableError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})
}
