package render_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/smartystreets/goconvey/convey"
	yaml "go.yaml.in/yaml/v3"

	"github.com/odiumuniverse/verger/pkg/render"
)

func TestRuleSkillLongSlug(t *testing.T) {
	Convey("Given a name whose slug exceeds the filesystem name bound", t, func() {
		long := strings.Repeat("я", 200) // 400 bytes

		rel, content, err := render.RuleSkill(long, []byte("Do it.\n"))

		Convey("When the rule is wrapped", func() {
			Convey("Then the slug is capped, unique and deterministic", func() {
				So(err, ShouldBeNil)

				name := strings.TrimPrefix(rel, "skills/")
				So(name, ShouldStartWith, "rule-")
				So(len(name), ShouldBeLessThanOrEqualTo, 255)
				So(len(name), ShouldBeLessThan, len("rule-")+len(long))
				So(skillNameOf(t, string(content)), ShouldEqual, name)

				other, _, otherErr := render.RuleSkill(long+"x", []byte("Do it.\n"))
				So(otherErr, ShouldBeNil)
				So(other, ShouldNotEqual, rel)

				again, _, _ := render.RuleSkill(long, []byte("Do it.\n"))
				So(again, ShouldEqual, rel)
			})
		})
	})
}

// descriptionOf decodes the frontmatter description of a rendered SKILL.md; the
// YAML emitter may quote or escape multibyte scalars, so the test must decode
// rather than read the raw line.
func descriptionOf(t *testing.T, content string) string {
	t.Helper()

	front, _, ok := strings.Cut(content, "\n---\n")
	if !ok {
		t.Fatalf("rendered skill has no closing frontmatter fence:\n%s", content)
	}

	var doc struct {
		Description string `yaml:"description"`
	}

	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		t.Fatalf("parse rendered frontmatter: %v", err)
	}

	return doc.Description
}

// skillNameOf decodes the frontmatter name of a rendered SKILL.md.
func skillNameOf(t *testing.T, content string) string {
	t.Helper()

	front, _, ok := strings.Cut(content, "\n---\n")
	if !ok {
		t.Fatalf("rendered skill has no closing frontmatter fence:\n%s", content)
	}

	var doc struct {
		Name string `yaml:"name"`
	}

	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		t.Fatalf("parse rendered frontmatter: %v", err)
	}

	return doc.Name
}

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
		cases := []struct {
			name string
			line string
		}{
			{"ascii", strings.Repeat("a", 130)},
			{"cyrillic", strings.Repeat("я", 130)},
			{"emoji", strings.Repeat("😀", 130)},
			{"mixed multibyte boundary", strings.Repeat("a", 119) + "€"},
		}

		for _, item := range cases {
			Convey("When the "+item.name+" line is truncated", func() {
				rel, content, err := render.RuleSkill("long", []byte(item.line+"\nbody\n"))

				Convey("Then exactly 120 runes survive as valid UTF-8", func() {
					So(err, ShouldBeNil)
					So(rel, ShouldEqual, "skills/rule-long")

					description := descriptionOf(t, string(content))

					So(description, ShouldEqual, string([]rune(item.line)[:120]))
					So(utf8.RuneCountInString(description), ShouldEqual, 120)
					So(utf8.ValidString(description), ShouldBeTrue)
				})
			})
		}
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
