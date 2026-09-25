package render_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/render"
)

func TestRuleSkill(t *testing.T) {
	Convey("Given a two-line rule", t, func() {
		rule := []byte("\nAlways review the diff carefully.\nSecond line.\n")

		rel, content, err := render.RuleSkill("Review Checklist", rule)

		Convey("When it is wrapped as a skill", func() {
			Convey("Then the golden SKILL.md carries the slug and description", func() {
				So(err, ShouldBeNil)
				So(rel, ShouldEqual, "skills/rule-review-checklist")
				So(string(content), ShouldEqual, `---
name: rule-review-checklist
description: Always review the diff carefully.
---
Always review the diff carefully.
Second line.
`)
			})
		})
	})

	Convey("Given a name with punctuation", t, func() {
		Convey("When the rule is wrapped", func() {
			rel, _, err := render.RuleSkill("  Acme/Rules: v2!  ", []byte("Do it.\n"))

			Convey("Then the slug is normalized", func() {
				So(err, ShouldBeNil)
				So(rel, ShouldEqual, "skills/rule-acme-rules-v2")
			})
		})
	})

	Convey("Given an unsluggable name", t, func() {
		Convey("When the rule is wrapped", func() {
			_, _, err := render.RuleSkill("!!!", []byte("Do it.\n"))

			_, ok := errors.AsType[*render.RenderError](err)

			Convey("Then it is refused", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a very long first line", t, func() {
		line := strings.Repeat("a", 130)

		rel, content, err := render.RuleSkill("long", []byte(line+"\nbody\n"))

		Convey("When the description is extracted", func() {
			Convey("Then it is truncated to 120 characters", func() {
				So(err, ShouldBeNil)
				So(rel, ShouldEqual, "skills/rule-long")

				description := ""

				for l := range strings.SplitSeq(string(content), "\n") {
					if after, ok := strings.CutPrefix(l, "description: "); ok {
						description = after
					}
				}

				So(len([]rune(description)), ShouldEqual, 120)
			})
		})
	})

	Convey("Given an empty rule", t, func() {
		forms := [][]byte{nil, {}, []byte("   \n\t\n")}

		for i, rule := range forms {
			Convey("When empty rule form "+strconv.Itoa(i)+" is wrapped", func() {
				_, _, err := render.RuleSkill("empty", rule)

				_, ok := errors.AsType[*render.RenderError](err)

				Convey("Then it is refused", func() {
					So(ok, ShouldBeTrue)
				})
			})
		}
	})

	Convey("Given a unicode rule", t, func() {
		Convey("When it is wrapped", func() {
			rel, content, err := render.RuleSkill("правила", []byte("Проверяй снег ☃.\n"))

			Convey("Then the description and body survive", func() {
				So(err, ShouldBeNil)
				So(rel, ShouldEqual, "skills/rule-правила")
				So(string(content), ShouldContainSubstring, "description: Проверяй снег ☃.")
				So(string(content), ShouldContainSubstring, "Проверяй снег ☃.")
			})
		})
	})
}
