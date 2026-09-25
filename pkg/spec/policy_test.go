package spec

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

const policySpec = `schema = 1

[propagate]
install = "origin"

[propagate.kind.hook]
install = "ask"

[propagate.host.codex]
install = "all"

[[package]]
id = "acme/kit"

[package.propagate]
install = "origin"
`

func TestEffectiveMode(t *testing.T) {
	Convey("Given a spec with layered policy", t, func() {
		spec := mustParse(t, []byte(policySpec))
		pkg := &spec.Packages[0]

		Convey("When the effective mode is resolved", func() {
			Convey("Then the package override wins over every level", func() {
				So(spec.EffectiveMode(EventInstall, "hook", "codex", pkg), ShouldEqual, ModeOrigin)
			})

			Convey("Then the host override wins over kind and global", func() {
				So(spec.EffectiveMode(EventInstall, "hook", "codex", nil), ShouldEqual, ModeAll)
			})

			Convey("Then the kind override wins over global", func() {
				So(spec.EffectiveMode(EventInstall, "hook", "claude", nil), ShouldEqual, ModeAsk)
			})

			Convey("Then global applies for unknown kind and host", func() {
				So(spec.EffectiveMode(EventInstall, "skill", "claude", nil), ShouldEqual, ModeOrigin)
			})

			Convey("Then events are independent", func() {
				So(spec.EffectiveMode(EventRemove, "", "", nil), ShouldEqual, ModeAll)
				So(spec.EffectiveMode(EventInstall, "", "", nil), ShouldEqual, ModeOrigin)
			})

			Convey("Then a nil package does not change the result", func() {
				So(spec.EffectiveMode(EventInstall, "", "", nil), ShouldEqual, spec.EffectiveMode(EventInstall, "", "", pkg))
			})
		})
	})

	Convey("Given a nil spec", t, func() {
		var spec *Spec

		Convey("When the effective mode is resolved", func() {
			Convey("Then everything propagates to all", func() {
				So(spec.EffectiveMode(EventInstall, "hook", "codex", nil), ShouldEqual, ModeAll)
			})
		})
	})
}

func TestPropagateMode(t *testing.T) {
	Convey("Given a policy with one event set", t, func() {
		policy := Propagate{Install: ModeAsk}

		Convey("When the event mode is read", func() {
			Convey("Then only the set event returns a mode", func() {
				So(policy.Mode(EventInstall), ShouldEqual, ModeAsk)
				So(policy.Mode(EventRemove), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a nil policy", t, func() {
		var policy *Propagate

		Convey("When the event mode is read", func() {
			Convey("Then it is empty and never panics", func() {
				So(policy.Mode(EventInstall), ShouldBeEmpty)
			})
		})
	})
}

func TestEventValid(t *testing.T) {
	Convey("Given every documented event", t, func() {
		events := []Event{
			EventInstall, EventRemove, EventDisable, EventEnable,
			EventUpdate, EventAdopt, EventMarketplace,
		}

		Convey("When they are validated", func() {
			Convey("Then they are valid and unknown events are not", func() {
				for _, ev := range events {
					So(ev.Valid(), ShouldBeTrue)
				}

				So(Event("bogus").Valid(), ShouldBeFalse)
			})
		})
	})
}

func TestCodeDefaults(t *testing.T) {
	Convey("Given a fresh spec", t, func() {
		spec := New()

		Convey("When defaults are read", func() {
			Convey("Then cooldown and hooks fall back to the code defaults", func() {
				So(spec.Schema, ShouldEqual, Schema)
				So(spec.Cooldown(), ShouldEqual, 24*time.Hour)
				So(spec.Cooldown(), ShouldEqual, DefaultCooldown)
				So(spec.HooksMode(), ShouldEqual, HooksAsk)
			})
		})
	})

	Convey("Given explicit defaults", t, func() {
		spec := New()
		spec.Defaults.Hooks = HooksNo
		spec.Defaults.Cooldown = Duration(90 * time.Minute)

		Convey("When defaults are read", func() {
			Convey("Then the set values win", func() {
				So(spec.Cooldown(), ShouldEqual, 90*time.Minute)
				So(spec.HooksMode(), ShouldEqual, HooksNo)
			})
		})
	})

	Convey("Given a nil spec", t, func() {
		var spec *Spec

		Convey("When defaults are read", func() {
			Convey("Then the code defaults apply", func() {
				So(spec.Cooldown(), ShouldEqual, DefaultCooldown)
				So(spec.HooksMode(), ShouldEqual, HooksAsk)
			})
		})
	})
}
