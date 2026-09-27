package spec

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	. "github.com/smartystreets/goconvey/convey"
)

const unknownFixture = `schema = 1
top_future = "keep"

[unknown_table]
note = "keep me"

[[unknown_table.items]]
key = "a"

[policy]
mode = "v2"

[defaults]
hooks = "ask"
cooldown = "1h"
defaults_future = true

[propagate]
install = "all"
propagate_future = "x"

[propagate.kind.hook]
adopt = "origin"
kind_future = 7

[propagate.host.codex]
disable = "origin"
host_future = "y"

[[source]]
name = "getverger"
source_future = "s"

[[package]]
id = "acme/kit"
future_field = 3

[package.future]
nested = 4

[[package.gen]]
x = 1

[[package.gen]]
x = 2

[package.propagate]
remove = "ask"
package_propagate_future = 5
`

const baseSpec = `schema = 1
[defaults]
hooks = "yes"
cooldown = "1h"
[[package]]
id = "a/b"
version = "1.0.0"
`

func TestParseDesignExample(t *testing.T) {
	Convey("Given the DESIGN §3.4 example fixture", t, func() {
		spec := mustParseFile(t, filepath.Join("testdata", "design.toml"))

		Convey("When it is parsed", func() {
			Convey("Then the schema and defaults are set", func() {
				So(spec.Schema, ShouldEqual, Schema)
				So(spec.Defaults.Hooks, ShouldEqual, HooksAsk)
				So(spec.Defaults.Cooldown, ShouldEqual, Duration(24*time.Hour))
				So(spec.Cooldown(), ShouldEqual, 24*time.Hour)
				So(spec.HooksMode(), ShouldEqual, HooksAsk)
			})

			Convey("Then all seven global propagation modes are parsed", func() {
				So(spec.Propagate.Install, ShouldEqual, ModeAll)
				So(spec.Propagate.Remove, ShouldEqual, ModeAll)
				So(spec.Propagate.Disable, ShouldEqual, ModeAll)
				So(spec.Propagate.Enable, ShouldEqual, ModeAll)
				So(spec.Propagate.Update, ShouldEqual, ModeAll)
				So(spec.Propagate.Adopt, ShouldEqual, ModeAll)
				So(spec.Propagate.Marketplace, ShouldEqual, ModeAll)
			})

			Convey("Then kind and host overrides are parsed", func() {
				So(spec.Propagate.Kind, ShouldHaveLength, 1)
				So(spec.Propagate.Kind["hook"].Adopt, ShouldEqual, ModeOrigin)
				So(spec.Propagate.Host, ShouldHaveLength, 1)
				So(spec.Propagate.Host["codex"].Disable, ShouldEqual, ModeOrigin)
			})

			Convey("Then sources keep their declared order", func() {
				So(spec.Sources, ShouldHaveLength, 2)
				So(spec.Sources[0].Name, ShouldEqual, "acme-internal")
				So(spec.Sources[0].URL, ShouldEqual, "git@github.com:acme/agent-plugins.git")
				So(spec.Sources[1].Name, ShouldEqual, "getverger")
				So(spec.Sources[1].URL, ShouldBeEmpty)
			})

			Convey("Then packages carry their options", func() {
				So(spec.Packages, ShouldHaveLength, 4)

				caveman := spec.Packages[0]
				So(caveman.ID, ShouldEqual, "JuliusBrussee/caveman")
				So(caveman.Channel, ShouldEqual, "stable")
				So(caveman.Except, ShouldResemble, []string{"pi"})

				So(spec.Packages[1].ID, ShouldEqual, "vercel-labs/skills//find-skills")

				mcp := spec.Packages[2]
				So(mcp.ID, ShouldEqual, "mcp:io.github.github/github-mcp-server")
				So(mcp.Env, ShouldResemble,
					map[string]string{"GITHUB_TOKEN": "{secret:GITHUB_TOKEN}"}) //nolint:gosec // G101: fixture value, not a credential

				kit := spec.Packages[3]
				So(kit.ID, ShouldEqual, "acme/review-kit")
				So(kit.Version, ShouldEqual, "2.3.1")
				So(kit.Disabled, ShouldBeTrue)
				So(kit.Propagate, ShouldNotBeNil)
				So(kit.Propagate.Remove, ShouldEqual, ModeAsk)
			})

			Convey("Then marshal, reparse and marshal are stable", func() {
				data := mustMarshal(t, spec)

				reparsed, err := Parse(data)
				So(err, ShouldBeNil)
				So(reparsed, ShouldResemble, spec)
				So(string(mustMarshal(t, reparsed)), ShouldEqual, string(data))

				So(string(data), ShouldContainSubstring, "[[source]]")
				So(string(data), ShouldContainSubstring, "[[package]]")
			})
		})
	})
}

func TestRoundTripUnknowns(t *testing.T) {
	Convey("Given a spec with unknown fields at every level", t, func() {
		spec := mustParse(t, []byte(unknownFixture))

		Convey("When it is marshalled twice through a reparse", func() {
			first := mustMarshal(t, spec)

			reparsed, err := Parse(first)
			So(err, ShouldBeNil)

			second := mustMarshal(t, reparsed)

			Convey("Then the second marshal is byte-identical", func() {
				So(string(second), ShouldEqual, string(first))
			})

			Convey("Then every unknown field survived at its level", func() {
				So(reparsed.raw["top_future"], ShouldEqual, "keep")
				So(reparsed.Defaults.raw["defaults_future"], ShouldEqual, true)
				So(reparsed.Propagate.raw["propagate_future"], ShouldEqual, "x")
				So(reparsed.Propagate.Kind["hook"].raw["kind_future"], ShouldEqual, int64(7))
				So(reparsed.Propagate.Host["codex"].raw["host_future"], ShouldEqual, "y")
				So(reparsed.Sources[0].raw["source_future"], ShouldEqual, "s")
				So(reparsed.Packages[0].raw["future_field"], ShouldEqual, int64(3))
				So(reparsed.Packages[0].raw["future"], ShouldResemble, map[string]any{"nested": int64(4)})

				unknown := asTable(t, reparsed.raw["unknown_table"])
				So(unknown["note"], ShouldEqual, "keep me")
				So(unknown["items"], ShouldHaveLength, 1)

				// The `policy` key is reserved for v2 (rule 12): it must
				// round-trip literally, not be dropped or validated.
				So(reparsed.raw["policy"], ShouldResemble, map[string]any{"mode": "v2"})
				So(string(second), ShouldContainSubstring, "[policy]")

				gen := asList(t, reparsed.Packages[0].raw["gen"])
				So(gen, ShouldHaveLength, 2)

				So(reparsed.Packages[0].Propagate, ShouldNotBeNil)
				So(reparsed.Packages[0].Propagate.raw["package_propagate_future"], ShouldEqual, int64(5))
			})
		})

		Convey("When known fields are edited", func() {
			spec.Defaults.Cooldown = Duration(2 * time.Hour)
			spec.Packages = append(spec.Packages, Package{ID: "added/pkg"})

			edited := mustMarshal(t, spec)

			reparsed, err := Parse(edited)
			So(err, ShouldBeNil)

			Convey("Then the edit applies and unknown fields stay", func() {
				So(reparsed.Cooldown(), ShouldEqual, 2*time.Hour)
				So(reparsed.Packages, ShouldHaveLength, 2)
				So(reparsed.raw["top_future"], ShouldEqual, "keep")
				So(reparsed.Propagate.raw["propagate_future"], ShouldEqual, "x")
				So(reparsed.Packages[0].raw["future_field"], ShouldEqual, int64(3))
				So(reparsed.Packages[1].raw, ShouldBeEmpty)
			})
		})
	})
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		check func(err error) bool
	}{
		{"missing schema", "[defaults]\nhooks = \"ask\"\n", isSchemaInvalid},
		{"zero schema", "schema = 0\n", isSchemaInvalid},
		{"negative schema", "schema = -1\n", isSchemaInvalid},
		{"newer schema", "schema = 2\n", isSchemaNewer},
		// The schema guard wins over value validation: a newer (or absent)
		// schema must be refused before a future cooldown representation is
		// judged (review T0.5 N1).
		{
			"newer schema beats numeric cooldown",
			"schema = 2\n[defaults]\ncooldown = 5\n",
			isSchemaNewer,
		},
		{
			"newer schema beats unparseable cooldown",
			"schema = 2\n[defaults]\ncooldown = \"soon\"\n",
			isSchemaNewer,
		},
		{
			"missing schema beats numeric cooldown",
			"[defaults]\ncooldown = 5\n",
			isSchemaInvalid,
		},
		{
			"zero schema beats numeric cooldown",
			"schema = 0\n[defaults]\ncooldown = 5\n",
			isSchemaInvalid,
		},
		{"invalid TOML", "schema = 1\n[[package]\nid = \"a/b\"\n", isDecodeError},
		{
			"duplicate package id",
			"schema = 1\n[[package]]\nid = \"a/b\"\n[[package]]\nid = \"a/b\"\n",
			isDuplicateID("package", "a/b"),
		},
		{
			"duplicate source name",
			"schema = 1\n[[source]]\nname = \"acme\"\n[[source]]\nname = \"acme\"\n",
			isDuplicateID("source", "acme"),
		},
		{"bad hooks mode", "schema = 1\n[defaults]\nhooks = \"maybe\"\n", isInvalidValue("defaults", "hooks")},
		{
			"bad cooldown text",
			"schema = 1\n[defaults]\ncooldown = \"soon\"\n",
			isInvalidValue("defaults", "cooldown"),
		},
		{
			"negative cooldown",
			"schema = 1\n[defaults]\ncooldown = \"-1h\"\n",
			isInvalidValue("defaults", "cooldown"),
		},
		{
			"non-string cooldown",
			"schema = 1\n[defaults]\ncooldown = 5\n",
			isInvalidValue("defaults", "cooldown"),
		},
		{
			"bad global mode",
			"schema = 1\n[propagate]\ninstall = \"somewhere\"\n",
			isInvalidValue("propagate", "install"),
		},
		{
			"bad kind mode",
			"schema = 1\n[propagate.kind.skill]\ninstall = \"somewhere\"\n",
			isInvalidValue("propagate.kind.skill", "install"),
		},
		{
			"bad host mode",
			"schema = 1\n[propagate.host.codex]\nremove = \"somewhere\"\n",
			isInvalidValue("propagate.host.codex", "remove"),
		},
		{
			"bad package mode",
			"schema = 1\n[[package]]\nid = \"a/b\"\n[package.propagate]\nupdate = \"somewhere\"\n",
			isInvalidValue("package.propagate", "update"),
		},
		{
			"empty except element",
			"schema = 1\n[[package]]\nid = \"a/b\"\nexcept = [\"\"]\n",
			isInvalidValue("package", "except"),
		},
		{
			"empty source name",
			"schema = 1\n[[source]]\nurl = \"https://example.com/x.git\"\n",
			isInvalidValue("source", "name"),
		},
		{
			"unsafe package id",
			"schema = 1\n[[package]]\nid = \"a/../b\"\n",
			isInvalidValue("package", "id"),
		},
		{
			"empty package id",
			"schema = 1\n[[package]]\nversion = \"1.0.0\"\n",
			isInvalidValue("package", "id"),
		},
	}

	for _, tc := range cases {
		Convey("Given the invalid spec "+tc.name, t, func() {
			_, err := Parse([]byte(tc.input))

			Convey("When it is parsed", func() {
				Convey("Then it fails with the typed error", func() {
					So(err, ShouldBeError)
					So(tc.check(err), ShouldBeTrue)
				})
			})
		})
	}
}

func isSchemaInvalid(err error) bool {
	_, ok := errors.AsType[*SchemaInvalidError](err)

	return ok
}

func isSchemaNewer(err error) bool {
	found, ok := errors.AsType[*SchemaNewerError](err)

	return ok && found.Found == 2 && found.Supported == Schema
}

func isDecodeError(err error) bool {
	_, ok := errors.AsType[*toml.DecodeError](err)

	return ok
}

func isDuplicateID(kind, id string) func(error) bool {
	return func(err error) bool {
		found, ok := errors.AsType[*DuplicateIDError](err)

		return ok && found.Kind == kind && found.ID == id
	}
}

func isInvalidValue(table, key string) func(error) bool {
	return func(err error) bool {
		found, ok := errors.AsType[*InvalidValueError](err)

		return ok && found.Table == table && found.Key == key
	}
}

func TestParseBOM(t *testing.T) {
	Convey("Given a spec with a leading UTF-8 BOM", t, func() {
		doc, err := Parse([]byte("\ufeffschema = 1\n[[package]]\nid = \"acme/kit\"\n"))

		Convey("When it is parsed", func() {
			Convey("Then the BOM is stripped and the document is normal", func() {
				So(err, ShouldBeNil)
				So(doc.Schema, ShouldEqual, Schema)
				So(doc.Packages, ShouldHaveLength, 1)
				So(doc.Packages[0].ID, ShouldEqual, "acme/kit")
			})

			Convey("Then marshalling does not resurrect the BOM", func() {
				data := mustMarshal(t, doc)
				So(strings.HasPrefix(string(data), "\ufeff"), ShouldBeFalse)

				reparsed, parseErr := Parse(data)
				So(parseErr, ShouldBeNil)
				So(reparsed.Packages[0].ID, ShouldEqual, "acme/kit")
			})
		})
	})

	Convey("Given a BOM followed by CRLF line endings", t, func() {
		doc, err := Parse([]byte("\ufeffschema = 1\r\n[defaults]\r\ncooldown = \"2h\"\r\n[[package]]\r\nid = \"acme/kit\"\r\n"))

		Convey("When it is parsed", func() {
			Convey("Then BOM and CRLF are both accepted", func() {
				So(err, ShouldBeNil)
				So(doc.Cooldown(), ShouldEqual, 2*time.Hour)
				So(doc.Packages[0].ID, ShouldEqual, "acme/kit")
			})
		})
	})

	Convey("Given a BOM inside a value", t, func() {
		_, err := Parse([]byte("schema = 1\n[[package]]\nid = \"acme/\ufeffkit\"\n"))

		Convey("When it is parsed", func() {
			Convey("Then only the leading BOM is special and the id is rejected", func() {
				So(err, ShouldBeError)
			})
		})
	})
}

func TestInvalidIDTaxonomy(t *testing.T) {
	Convey("Given an unsafe package id inside a spec", t, func() {
		_, err := Parse([]byte("schema = 1\n[[package]]\nid = \"a/../b\"\n"))

		value, valueOK := errors.AsType[*InvalidValueError](err)
		_, idOK := errors.AsType[*InvalidIDError](err)

		Convey("When Parse validates it", func() {
			Convey("Then it is reported as InvalidValueError without an InvalidIDError chain", func() {
				So(valueOK, ShouldBeTrue)
				So(value.Table, ShouldEqual, "package")
				So(value.Key, ShouldEqual, "id")
				So(value.Value, ShouldEqual, "a/../b")
				So(value.Reason, ShouldContainSubstring, "parent directory segment")
				So(idOK, ShouldBeFalse)
			})
		})
	})

	Convey("Given ValidateID called directly", t, func() {
		_, ok := errors.AsType[*InvalidIDError](ValidateID("a/../b"))

		Convey("When it rejects the id", func() {
			Convey("Then it returns InvalidIDError", func() {
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestParseFile(t *testing.T) {
	Convey("Given a missing spec file", t, func() {
		path := filepath.Join(t.TempDir(), "missing.toml")

		Convey("When it is parsed", func() {
			_, err := ParseFile(path)

			Convey("Then the OS error is wrapped", func() {
				So(errors.Is(err, fs.ErrNotExist), ShouldBeTrue)
			})
		})
	})

	Convey("Given a file with a newer schema", t, func() {
		path := filepath.Join(t.TempDir(), "verger.toml")
		mustWriteFile(t, path, []byte("schema = 2\n"))

		Convey("When it is parsed", func() {
			_, err := ParseFile(path)

			Convey("Then the error names the file", func() {
				found, ok := errors.AsType[*SchemaNewerError](err)
				So(ok, ShouldBeTrue)
				So(found.Path, ShouldEqual, path)
				So(found.Found, ShouldEqual, 2)
			})
		})
	})
}

func TestDuration(t *testing.T) {
	Convey("Given duration strings", t, func() {
		cases := []struct {
			text string
			want time.Duration
			out  string
		}{
			{"24h", 24 * time.Hour, "24h"},
			{"1h30m", 90 * time.Minute, "1h30m"},
			{"90m", 90 * time.Minute, "1h30m"},
			{"30s", 30 * time.Second, "30s"},
			{"0s", 0, "0s"},
		}

		for _, tc := range cases {
			Convey("Given the duration "+tc.text, func() {
				var dur Duration

				Convey("When it is parsed and marshalled back", func() {
					err := dur.UnmarshalText([]byte(tc.text))

					Convey("Then value and canonical text round-trip", func() {
						So(err, ShouldBeNil)
						So(dur.Duration(), ShouldEqual, tc.want)

						text, marshalErr := dur.MarshalText()
						So(marshalErr, ShouldBeNil)
						So(string(text), ShouldEqual, tc.out)

						var back Duration
						So(back.UnmarshalText(text), ShouldBeNil)
						So(back, ShouldEqual, dur)
					})
				})
			})
		}

		Convey("Given malformed or negative durations", func() {
			for _, bad := range []string{"soon", "", "24", "-1h"} {
				Convey("Given the bad duration "+bad, func() {
					var dur Duration

					Convey("When it is parsed", func() {
						err := dur.UnmarshalText([]byte(bad))

						Convey("Then it fails", func() {
							So(err, ShouldBeError)
						})
					})
				})
			}
		})
	})
}

func TestSave(t *testing.T) {
	Convey("Given a spec to save", t, func() {
		dir := t.TempDir()
		spec := New()
		spec.Packages = []Package{{ID: "acme/kit"}}

		Convey("When it is saved as a new file", func() {
			path := filepath.Join(dir, "verger.toml")
			err := spec.Save(path)
			info, statErr := os.Stat(path)

			Convey("Then it is 0600 and no temp file remains", func() {
				So(err, ShouldBeNil)
				So(statErr, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, fs.FileMode(0o600))

				leftovers, globErr := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
				So(globErr, ShouldBeNil)
				So(leftovers, ShouldBeEmpty)
			})
		})

		Convey("When the file already exists with mode 0640", func() {
			path := filepath.Join(dir, "existing.toml")
			mustWriteFile(t, path, []byte("schema = 1\n"))
			So(os.Chmod(path, 0o640), ShouldBeNil) //nolint:gosec // G302: pins that an existing mode is preserved

			Convey("Then saving keeps the existing mode", func() {
				So(spec.Save(path), ShouldBeNil)

				info, err := os.Stat(path)
				So(err, ShouldBeNil)
				So(info.Mode().Perm(), ShouldEqual, fs.FileMode(0o640))
			})
		})

		Convey("When the parent directory is missing", func() {
			path := filepath.Join(dir, "missing", "verger.toml")

			Convey("Then saving fails", func() {
				So(spec.Save(path), ShouldBeError)
			})
		})
	})
}

func TestSaveRefusesOutOfRangeSchema(t *testing.T) {
	Convey("Given hand-built specs with an out-of-range schema", t, func() {
		dir := t.TempDir()

		Convey("When a newer schema is saved", func() {
			path := filepath.Join(dir, "newer.toml")
			doc := New()
			doc.Schema = Schema + 1

			err := doc.Save(path)

			_, statErr := os.Stat(path)

			Convey("Then it is refused with *SchemaNewerError, Path set and nothing written", func() {
				detail, ok := errors.AsType[*SchemaNewerError](err)
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Path, ShouldEqual, path)
					So(detail.Found, ShouldEqual, Schema+1)
					So(detail.Supported, ShouldEqual, Schema)
				}

				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)
			})
		})

		Convey("When a zero schema is saved", func() {
			path := filepath.Join(dir, "zero.toml")
			doc := &Spec{}

			err := doc.Save(path)

			_, statErr := os.Stat(path)

			Convey("Then it is refused with *SchemaInvalidError, Path set and nothing written", func() {
				detail, ok := errors.AsType[*SchemaInvalidError](err)
				So(ok, ShouldBeTrue)

				if ok {
					So(detail.Path, ShouldEqual, path)
					So(detail.Found, ShouldEqual, 0)
				}

				So(errors.Is(statErr, fs.ErrNotExist), ShouldBeTrue)
			})
		})

		Convey("When an existing file would be replaced by an out-of-range schema", func() {
			path := filepath.Join(dir, "existing.toml")
			So(New().Save(path), ShouldBeNil)

			before := mustReadFile(t, path)

			bad := New()
			bad.Schema = Schema + 1

			err := bad.Save(path)

			Convey("Then the stored bytes stay untouched", func() {
				So(err, ShouldBeError)
				So(mustReadFile(t, path), ShouldResemble, before)
			})
		})
	})
}

func TestSaveWritesNoEnvironmentPaths(t *testing.T) {
	Convey("Given a spec loaded from a file", t, func() {
		source := filepath.Join(t.TempDir(), "in", "verger.toml")
		mustWriteFile(t, source, []byte(unknownFixture))

		spec := mustParseFile(t, source)

		target := filepath.Join(t.TempDir(), "out", "verger.toml")
		So(os.MkdirAll(filepath.Dir(target), 0o700), ShouldBeNil)

		Convey("When verger saves it elsewhere", func() {
			So(spec.Save(target), ShouldBeNil)
			data := mustReadFile(t, target)

			Convey("Then verger writes no absolute path of its own", func() {
				So(string(data), ShouldNotContainSubstring, filepath.Dir(source))
				So(string(data), ShouldNotContainSubstring, filepath.Dir(target))
			})
		})
	})
}

func TestEnvValuesAreOpaque(t *testing.T) {
	Convey("Given a package with a non-secret env reference", t, func() {
		spec := mustParse(t, []byte("schema = 1\n[[package]]\nid = \"a/b\"\nenv = { TOKEN = \"{env:TOKEN}\" }\n"))

		Convey("When it is parsed", func() {
			Convey("Then T0.5 keeps the value verbatim; secret-ref syntax is T1.1's", func() {
				So(spec.Packages[0].Env["TOKEN"], ShouldEqual, "{env:TOKEN}")
			})
		})
	})
}

func TestDigest(t *testing.T) {
	Convey("Given two equal specs in different layouts", t, func() {
		first := mustParse(t, []byte(baseSpec))
		second := mustParse(t, []byte(`# a comment changes nothing
schema = 1

[[package]]
version = "1.0.0"
id    = "a/b"

[defaults]
cooldown = "1h"
hooks    = "yes"
`))

		Convey("When their digests are computed", func() {
			firstHash := first.Digest()
			secondHash := second.Digest()

			Convey("Then comments and key order do not matter", func() {
				So(firstHash, ShouldEqual, secondHash)
				So(firstHash.String(), ShouldHaveLength, 64)
			})
		})
	})

	cases := []struct {
		name  string
		input string
	}{
		{"added package", baseSpec + "[[package]]\nid = \"c/d\"\n"},
		{"changed version", strings.Replace(baseSpec, `version = "1.0.0"`, `version = "1.0.1"`, 1)},
		{"disabled package", strings.Replace(baseSpec, `id = "a/b"`, "id = \"a/b\"\ndisabled = true", 1)},
		{"changed cooldown", strings.Replace(baseSpec, `cooldown = "1h"`, `cooldown = "2h"`, 1)},
		{"changed hooks", strings.Replace(baseSpec, `hooks = "yes"`, `hooks = "no"`, 1)},
		{"package env", strings.Replace(baseSpec, `id = "a/b"`, "id = \"a/b\"\nenv = { TOKEN = \"plain\" }", 1)},
		{"unknown field", "mystery = 1\n" + baseSpec},
	}

	Convey("Given semantic mutations of a base spec", t, func() {
		baseHash := mustParse(t, []byte(baseSpec)).Digest()

		for _, tc := range cases {
			Convey("Given the mutation "+tc.name, func() {
				Convey("When its digest is computed", func() {
					mutated := mustParse(t, []byte(tc.input)).Digest()

					Convey("Then it differs", func() {
						So(mutated, ShouldNotEqual, baseHash)
					})
				})
			})
		}
	})
}

func TestConcurrency(t *testing.T) {
	fixture := filepath.Join("testdata", "design.toml")
	data := mustReadFile(t, fixture)

	Convey("Given one parsed spec shared by goroutines", t, func() {
		spec := mustParseFile(t, fixture)
		want := mustMarshal(t, spec)

		Convey("When many goroutines marshal it", func() {
			const workers = 8

			results := make([][]byte, workers)
			errs := make([]error, workers)

			var wg sync.WaitGroup

			for i := range workers {
				wg.Go(func() {
					results[i], errs[i] = spec.Marshal()
				})
			}

			wg.Wait()

			Convey("Then every result equals the canonical bytes", func() {
				for i := range workers {
					So(errs[i], ShouldBeNil)
					So(string(results[i]), ShouldEqual, string(want))
				}
			})
		})

		Convey("When many goroutines parse the fixture concurrently", func() {
			const workers = 8

			specs := make([]*Spec, workers)
			errs := make([]error, workers)

			var wg sync.WaitGroup

			for i := range workers {
				wg.Go(func() {
					specs[i], errs[i] = Parse(data)
				})
			}

			wg.Wait()

			Convey("Then every parse yields the same digest", func() {
				for i := range workers {
					So(errs[i], ShouldBeNil)
					So(specs[i].Digest(), ShouldEqual, spec.Digest())
				}
			})
		})
	})
}

func TestValidateID(t *testing.T) {
	Convey("Given safe package ids", t, func() {
		for _, id := range []string{
			"acme/kit",
			"JuliusBrussee/caveman",
			"vercel-labs/skills//find-skills",
			"mcp:io.github.github/github-mcp-server",
			"claude:caveman@caveman",
			"a",
			"a//b",
		} {
			Convey("Given the id "+id, func() {
				Convey("When it is validated", func() {
					Convey("Then it is accepted", func() {
						So(ValidateID(id), ShouldBeNil)
					})
				})
			})
		}
	})

	Convey("Given unsafe package ids", t, func() {
		for _, id := range []string{
			"",
			".",
			"/absolute",
			"./local/path",
			"trailing/",
			"a/../b",
			"..",
			"a b",
			"a\tb",
			`a\b`,
			"a\x00b",
			"a*b",
			"a//",
			"//x",
			"a///b",
			"a//b//c",
		} {
			Convey("Given the id "+strings.ReplaceAll(id, "\x00", "<NUL>"), func() {
				Convey("When it is validated", func() {
					_, ok := errors.AsType[*InvalidIDError](ValidateID(id))

					Convey("Then it is rejected with InvalidIDError", func() {
						So(ok, ShouldBeTrue)
					})
				})
			})
		}
	})
}
