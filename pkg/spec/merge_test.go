package spec

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

const mergeUserSpec = `schema = 1
top_future = "user"

[defaults]
hooks = "no"
cooldown = "1h"
defaults_future = "user"

[propagate]
install = "origin"
remove = "ask"
shared = "user"

[propagate.kind.skill]
install = "ask"

[propagate.host.codex]
disable = "ask"
remove = "ask"

[[source]]
name = "user-src"
url = "https://example.com/user.git"

[[source]]
name = "shared-src"
url = "https://example.com/user-shared.git"

[[source]]
name = "getverger"

[[package]]
id = "acme/kit"
version = "1.0.0"
except = ["pi"]
disabled = true
env = { TOKEN = "{secret:TOKEN}" }

[[package]]
id = "user/only"
`

const mergeProjectSpec = `schema = 1
top_future = "project"

[defaults]
hooks = "yes"
defaults_future = "project"

[propagate]
install = "all"
remove = "origin"
shared = "project"

[propagate.host.codex]
enable = "origin"

[propagate.host.claude]
install = "ask"

[[source]]
name = "project-src"
url = "https://example.com/project.git"

[[source]]
name = "shared-src"

[[package]]
id = "acme/kit"
version = "2.0.0"

[[package]]
id = "project/only"
`

func TestMergeNil(t *testing.T) {
	Convey("Given missing documents", t, func() {
		user := mustParse(t, []byte(mergeUserSpec))
		project := mustParse(t, []byte(mergeProjectSpec))

		Convey("When both are missing", func() {
			merged, err := Merge(nil, nil)

			Convey("Then a fresh schema-1 spec is returned", func() {
				So(err, ShouldBeNil)
				So(merged, ShouldNotBeNil)
				So(merged.Schema, ShouldEqual, Schema)
			})
		})

		Convey("When only the project is present", func() {
			merged, err := Merge(nil, project)

			Convey("Then the project spec is kept whole", func() {
				So(err, ShouldBeNil)
				So(merged, ShouldResemble, project)
			})
		})

		Convey("When only the user spec is present", func() {
			merged, err := Merge(user, nil)

			Convey("Then the user spec is kept whole", func() {
				So(err, ShouldBeNil)
				So(merged, ShouldResemble, user)
			})
		})
	})
}

func TestMergeDefaults(t *testing.T) {
	Convey("Given defaults in both scopes", t, func() {
		merged, err := Merge(mustParse(t, []byte(mergeUserSpec)), mustParse(t, []byte(mergeProjectSpec)))
		So(err, ShouldBeNil)

		Convey("When they are merged", func() {
			Convey("Then project fields win and user fields fill the gaps", func() {
				So(merged.Defaults.Hooks, ShouldEqual, HooksYes)
				So(merged.Cooldown(), ShouldEqual, time.Hour)
			})

			Convey("Then unknown defaults keys merge per key with project priority", func() {
				So(merged.Defaults.raw["defaults_future"], ShouldEqual, "project")
			})

			Convey("Then unknown top-level keys merge per key with project priority", func() {
				So(merged.raw["top_future"], ShouldEqual, "project")
			})
		})
	})
}

func TestMergePropagate(t *testing.T) {
	Convey("Given layered policies in both scopes", t, func() {
		merged, err := Merge(mustParse(t, []byte(mergeUserSpec)), mustParse(t, []byte(mergeProjectSpec)))
		So(err, ShouldBeNil)

		Convey("When they are merged", func() {
			Convey("Then global values take the project when set", func() {
				So(merged.Propagate.Install, ShouldEqual, ModeAll)
				So(merged.Propagate.Remove, ShouldEqual, ModeOrigin)
				So(merged.Propagate.Update, ShouldBeEmpty) // unset until EffectiveMode
				So(merged.EffectiveMode(EventUpdate, "", "", nil), ShouldEqual, ModeAll)
			})

			Convey("Then the project global beats a user host exception", func() {
				So(merged.Propagate.Host["codex"].Remove, ShouldEqual, ModeOrigin)
			})

			Convey("Then the project global beats a user kind exception", func() {
				So(merged.Propagate.Kind["skill"].Install, ShouldEqual, ModeAll)
				So(merged.Propagate.Kind["skill"].Remove, ShouldEqual, ModeOrigin)
			})

			Convey("Then user-only levels survive where the project is silent", func() {
				So(merged.Propagate.Host["codex"].Disable, ShouldEqual, ModeAsk)
				So(merged.Propagate.Host["codex"].Enable, ShouldEqual, ModeOrigin)
				So(merged.Propagate.Host["claude"].Install, ShouldEqual, ModeAsk)
			})

			Convey("Then unknown propagate keys merge per key", func() {
				So(merged.Propagate.raw["shared"], ShouldEqual, "project")
			})
		})
	})
}

func TestMergeSources(t *testing.T) {
	Convey("Given sources in both scopes", t, func() {
		merged, err := Merge(mustParse(t, []byte(mergeUserSpec)), mustParse(t, []byte(mergeProjectSpec)))
		So(err, ShouldBeNil)

		Convey("When they are merged", func() {
			Convey("Then project sources come first, dedupe by name and keep priority", func() {
				names := make([]string, 0, len(merged.Sources))
				for _, src := range merged.Sources {
					names = append(names, src.Name)
				}

				So(names, ShouldResemble, []string{"project-src", "shared-src", "user-src", "getverger"})
				So(merged.Sources[1].URL, ShouldBeEmpty) // the project's shared-src wins
			})
		})
	})
}

func TestMergePackages(t *testing.T) {
	Convey("Given packages in both scopes", t, func() {
		merged, err := Merge(mustParse(t, []byte(mergeUserSpec)), mustParse(t, []byte(mergeProjectSpec)))
		So(err, ShouldBeNil)

		Convey("When they are merged", func() {
			Convey("Then project packages lead and a shared id is replaced wholesale", func() {
				ids := make([]string, 0, len(merged.Packages))
				for _, pkg := range merged.Packages {
					ids = append(ids, pkg.ID)
				}

				So(ids, ShouldResemble, []string{"acme/kit", "project/only", "user/only"})

				kit := merged.Packages[0]
				So(kit.Version, ShouldEqual, "2.0.0")
				So(kit.Except, ShouldBeEmpty)
				So(kit.Disabled, ShouldBeFalse)
				So(kit.Env, ShouldBeEmpty)
			})
		})
	})
}

func TestMergeDoesNotAlias(t *testing.T) {
	Convey("Given merged specs", t, func() {
		user := mustParse(t, []byte(mergeUserSpec))
		project := mustParse(t, []byte(mergeProjectSpec))

		merged, err := Merge(user, project)
		So(err, ShouldBeNil)

		Convey("When the merged spec is mutated", func() {
			merged.Packages[0].ID = "mutated/kit"
			merged.Sources[0].Name = "mutated-src"
			merged.Propagate.Host["codex"] = Propagate{Disable: ModeAll}
			merged.Propagate.Kind["skill"] = Propagate{Install: ModeOrigin}
			merged.raw["top_future"] = "mutated"

			Convey("Then neither original spec changes", func() {
				So(project.Packages[0].ID, ShouldEqual, "acme/kit")
				So(project.Sources[0].Name, ShouldEqual, "project-src")
				So(project.Propagate.Host["codex"].Enable, ShouldEqual, ModeOrigin)
				So(user.Propagate.Host["codex"].Disable, ShouldEqual, ModeAsk)
				So(user.Propagate.Kind["skill"].Install, ShouldEqual, ModeAsk)
				So(user.raw["top_future"], ShouldEqual, "user")
				So(project.raw["top_future"], ShouldEqual, "project")
			})
		})
	})
}
