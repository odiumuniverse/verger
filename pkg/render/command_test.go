package render_test

import (
	"errors"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/odiumuniverse/verger/pkg/render"
)

func TestParseCommandMarkdown(t *testing.T) {
	Convey("Given a full command document", t, func() {
		data := []byte(`---
name: review
description: Review the diff
argument-hint: "<pr>"
arguments:
  - pr
model: sonnet
disable-model-invocation: true
unknown-key: dropped
---
Review $1 please.
`)

		Convey("When it is parsed", func() {
			cmd, err := render.ParseCommandMarkdown(data)

			Convey("Then the canonical fields are read and unknown keys dropped", func() {
				So(err, ShouldBeNil)
				So(cmd.Name, ShouldEqual, "review")
				So(cmd.Description, ShouldEqual, "Review the diff")
				So(cmd.ArgumentHint, ShouldEqual, "<pr>")
				So(cmd.Arguments, ShouldResemble, []string{"pr"})
				So(cmd.Model, ShouldEqual, "sonnet")
				So(cmd.DisableModelInvocation, ShouldResemble, new(true))
				So(cmd.Body, ShouldEqual, "Review $1 please.\n")
			})
		})
	})

	Convey("Given documents without frontmatter", t, func() {
		Convey("When it is parsed", func() {
			cmd, err := render.ParseCommandMarkdown([]byte("Just a prompt\n"))

			Convey("Then only the body is set", func() {
				So(err, ShouldBeNil)
				So(cmd, ShouldResemble, render.Command{Body: "Just a prompt\n"})
			})
		})
	})

	Convey("Given a document with an empty frontmatter block", t, func() {
		Convey("When it is parsed", func() {
			cmd, err := render.ParseCommandMarkdown([]byte("---\n---\nBody\n"))

			Convey("Then the body survives", func() {
				So(err, ShouldBeNil)
				So(cmd.Body, ShouldEqual, "Body\n")
			})
		})
	})

	Convey("Given CRLF line endings", t, func() {
		data := []byte("---\r\ndescription: Note\r\n---\r\nline1\r\nline2\r\n")

		Convey("When it is parsed", func() {
			cmd, err := render.ParseCommandMarkdown(data)

			Convey("Then the frontmatter is read and the body keeps its newlines", func() {
				So(err, ShouldBeNil)
				So(cmd.Description, ShouldEqual, "Note")
				So(cmd.Body, ShouldEqual, "line1\r\nline2\n")
			})
		})
	})

	Convey("Given arguments in different YAML forms", t, func() {
		cases := map[string]string{
			"block list":     "arguments:\n  - a\n  - b\n",
			"inline list":    "arguments: [a, b]\n",
			"scalar list":    "arguments: a, b\n",
			"scalar one":     "arguments: a\n",
			"empty sequence": "arguments: []\n",
		}

		for name, front := range cases {
			Convey("When "+name+" is parsed", func() {
				cmd, err := render.ParseCommandMarkdown([]byte("---\n" + front + "---\nBody\n"))

				Convey("Then the list is normalized", func() {
					So(err, ShouldBeNil)

					switch name {
					case "empty sequence":
						So(cmd.Arguments, ShouldBeEmpty)
					case "scalar one":
						So(cmd.Arguments, ShouldResemble, []string{"a"})
					default:
						So(cmd.Arguments, ShouldResemble, []string{"a", "b"})
					}
				})
			})
		}
	})

	Convey("Given malformed frontmatter", t, func() {
		Convey("When it is parsed", func() {
			_, err := render.ParseCommandMarkdown([]byte("---\ndescription: [unclosed\n---\nBody\n"))

			target, ok := errors.AsType[*render.RenderError](err)

			Convey("Then the typed render error is reported", func() {
				So(ok, ShouldBeTrue)
				So(target.Kind, ShouldEqual, "command")
			})
		})
	})
}

func TestCommandMarkdown(t *testing.T) {
	Convey("Given a canonical command", t, func() {
		cmd := render.Command{
			Name:                   "review",
			Description:            "Review the diff",
			ArgumentHint:           "pr-number",
			Arguments:              []string{"pr"},
			Model:                  "sonnet",
			DisableModelInvocation: new(false),
			Body:                   "Review $1 please.\n",
		}

		Convey("When it is rendered", func() {
			out := cmd.Markdown()

			Convey("Then the golden document carries the canonical order and no name", func() {
				So(string(out), ShouldEqual, `---
description: Review the diff
argument-hint: pr-number
arguments: [pr]
model: sonnet
disable-model-invocation: false
---
Review $1 please.
`)
			})
		})

		Convey("When it is parsed back", func() {
			back, err := render.ParseCommandMarkdown(cmd.Markdown())

			Convey("Then the document round-trips", func() {
				So(err, ShouldBeNil)
				So(back.Name, ShouldBeEmpty)
				So(back.Description, ShouldEqual, cmd.Description)
				So(back.ArgumentHint, ShouldEqual, cmd.ArgumentHint)
				So(back.Arguments, ShouldResemble, cmd.Arguments)
				So(back.Model, ShouldEqual, cmd.Model)
				So(back.DisableModelInvocation, ShouldResemble, cmd.DisableModelInvocation)
				So(back.Body, ShouldEqual, cmd.Body)
			})
		})
	})

	Convey("Given a body-only command", t, func() {
		Convey("When it is rendered", func() {
			Convey("Then no frontmatter block is written", func() {
				So(string(render.Command{Body: "hello\n"}.Markdown()), ShouldEqual, "hello\n")
			})
		})
	})

	Convey("Given unicode and YAML-significant values", t, func() {
		cmd := render.Command{
			Description: "Проверка: \"quotes\" & <tags>",
			Model:       "claude-sonnet-4-5",
			Body:        "тело\n",
		}

		Convey("When it is rendered and parsed back", func() {
			back, err := render.ParseCommandMarkdown(cmd.Markdown())

			Convey("Then the values round-trip", func() {
				So(err, ShouldBeNil)
				So(back.Description, ShouldEqual, cmd.Description)
				So(back.Model, ShouldEqual, cmd.Model)
				So(back.Body, ShouldEqual, cmd.Body)
			})
		})
	})
}

func TestCommandCodexPrompt(t *testing.T) {
	Convey("Given a command with frontmatter", t, func() {
		cmd := render.Command{Description: "Note", Body: "Prompt body\n"}

		Convey("When the Codex prompt is rendered", func() {
			Convey("Then it is the markdown body only", func() {
				So(string(cmd.CodexPrompt()), ShouldEqual, "Prompt body\n")
			})
		})
	})

	Convey("Given an empty command", t, func() {
		Convey("When the Codex prompt is rendered", func() {
			Convey("Then it is empty", func() {
				So(render.Command{}.CodexPrompt(), ShouldBeEmpty)
			})
		})
	})
}

func TestCommandGeminiTOML(t *testing.T) {
	Convey("Given a command with a description and body", t, func() {
		cmd := render.Command{Name: "review", Description: "Review the diff", Body: "Do it.\n"}

		Convey("When the Gemini TOML is rendered", func() {
			out, err := cmd.GeminiTOML()

			Convey("Then the golden TOML carries description and prompt", func() {
				So(err, ShouldBeNil)
				So(string(out), ShouldEqual, `description = "Review the diff"
prompt = """
Do it.
"""
`)
			})

			Convey("Then the TOML parses back into the same values", func() {
				var doc struct {
					Description string `toml:"description"`
					Prompt      string `toml:"prompt"`
				}

				unmarshalErr := toml.Unmarshal(out, &doc)

				So(unmarshalErr, ShouldBeNil)
				So(doc.Description, ShouldEqual, cmd.Description)
				So(doc.Prompt, ShouldEqual, "Do it.\n")
			})
		})
	})

	Convey("Given a command without a description", t, func() {
		Convey("When the Gemini TOML is rendered", func() {
			out, err := cmdBody("Prompt only")

			Convey("Then only the prompt is written", func() {
				So(err, ShouldBeNil)
				So(strings.HasPrefix(string(out), "prompt = "), ShouldBeTrue)
				So(strings.Contains(string(out), "description"), ShouldBeFalse)
			})
		})
	})

	Convey("Given a body-less command", t, func() {
		Convey("When the Gemini TOML is rendered", func() {
			_, err := render.Command{Name: "empty"}.GeminiTOML()

			target, ok := errors.AsType[*render.InexpressibleError](err)

			Convey("Then it is inexpressible for Gemini", func() {
				So(ok, ShouldBeTrue)
				So(target.Kind, ShouldEqual, "command")
				So(target.Name, ShouldEqual, "empty")
				So(target.Field, ShouldEqual, "prompt")
			})
		})
	})

	Convey("Given a body with quotes, backslashes and unicode", t, func() {
		body := "say \"hi\" \\ and\tснег ☃\r\nnext\n"
		cmd := render.Command{Body: body}

		Convey("When the Gemini TOML is rendered", func() {
			out, err := cmd.GeminiTOML()

			var doc struct {
				Prompt string `toml:"prompt"`
			}

			unmarshalErr := toml.Unmarshal(out, &doc)

			Convey("Then the prompt round-trips through TOML", func() {
				So(err, ShouldBeNil)
				So(unmarshalErr, ShouldBeNil)
				So(doc.Prompt, ShouldEqual, "say \"hi\" \\ and\tснег ☃\r\nnext\n")
			})
		})
	})
}

// cmdBody renders the Gemini TOML of a body-only command.
func cmdBody(body string) ([]byte, error) {
	return render.Command{Body: body}.GeminiTOML()
}

func TestLiftGeminiCommand(t *testing.T) {
	Convey("Given a Gemini command TOML", t, func() {
		data := []byte("prompt = \"\"\"\nDo it.\n\"\"\"\ndescription = \"Review\"\n")

		Convey("When it is lifted", func() {
			out, ok, err := render.LiftGeminiCommand("review", data)

			Convey("Then it becomes canonical markdown", func() {
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)

				cmd, parseErr := render.ParseCommandMarkdown(out)
				So(parseErr, ShouldBeNil)
				So(cmd.Description, ShouldEqual, "Review")
				So(cmd.Body, ShouldEqual, "Do it.\n")
			})

			Convey("Then the lifted document renders the Gemini TOML back", func() {
				cmd, parseErr := render.ParseCommandMarkdown(out)
				So(parseErr, ShouldBeNil)

				cmd.Name = "review"

				tomlOut, tomlErr := cmd.GeminiTOML()
				So(tomlErr, ShouldBeNil)
				So(string(tomlOut), ShouldEqual, "description = \"Review\"\nprompt = \"\"\"\nDo it.\n\"\"\"\n")
			})
		})
	})

	Convey("Given a Gemini command without a prompt", t, func() {
		Convey("When it is lifted", func() {
			out, ok, err := render.LiftGeminiCommand("review", []byte("description = \"x\"\n"))

			Convey("Then it is not a command", func() {
				So(err, ShouldBeError)
				So(ok, ShouldBeFalse)
				So(out, ShouldBeNil)
			})
		})
	})

	Convey("Given malformed TOML", t, func() {
		Convey("When it is lifted", func() {
			_, ok, err := render.LiftGeminiCommand("review", []byte("prompt = "))

			Convey("Then the parse error is reported", func() {
				So(err, ShouldBeError)
				So(ok, ShouldBeFalse)
			})
		})
	})

	Convey("Given a name that is not a command slug", t, func() {
		Convey("When it is lifted", func() {
			out, ok, err := render.LiftGeminiCommand("Not A Slug", []byte("prompt = \"x\"\n"))

			Convey("Then it is ignored without error", func() {
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)
				So(out, ShouldBeNil)
			})
		})
	})
}
