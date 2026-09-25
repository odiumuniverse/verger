package lock_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/lock"
)

func TestValidateCell(t *testing.T) {
	Convey("Given valid cells", t, func() {
		cases := []lock.Cell{
			{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyNative},
			{Package: "acme/x", Host: "codex", Scope: lock.ScopeProject, Version: "1.0.0", Strategy: lock.StrategySynth},
			{Package: "acme/x", Host: "gemini", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyLoose},
			{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Strategy: lock.StrategySilenced, Reason: "hooks-unapproved"},
			{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyNative, Digest: digest.Hash(treeHex)},
		}

		for index, cell := range cases {
			Convey(fmt.Sprintf("When cell %d (%s %s/%s) is validated", index, cell.Strategy, cell.Host, cell.Scope), func() {
				Convey("Then it passes", func() {
					So(lock.ValidateCell(cell), ShouldBeNil)
				})
			})
		}
	})

	Convey("Given invalid cells", t, func() {
		cases := map[string]lock.Cell{
			"empty package":           {Host: "claude", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative},
			"empty host":              {Package: "acme/x", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative},
			"bad scope":               {Package: "acme/x", Host: "claude", Scope: "global", Version: "1", Strategy: lock.StrategyNative},
			"bad strategy":            {Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1", Strategy: "magic"},
			"silenced without reason": {Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Strategy: lock.StrategySilenced},
			"native without version":  {Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Strategy: lock.StrategyNative},
			"bad digest":              {Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative, Digest: "nothex"},
			"uppercase digest":        {Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative, Digest: digest.Hash(strings.ToUpper(treeHex))},
		}

		for name, cell := range cases {
			Convey("When "+name+" is validated", func() {
				err := lock.ValidateCell(cell)

				detail, ok := errors.AsType[*lock.InvalidCellError](err)

				Convey("Then InvalidCellError names the cell", func() {
					So(ok, ShouldBeTrue)

					if ok {
						So(detail.Package, ShouldEqual, cell.Package)
						So(detail.Host, ShouldEqual, cell.Host)
						So(detail.Scope, ShouldEqual, cell.Scope)
					}
				})
			})
		}
	})
}

func TestCellLookup(t *testing.T) {
	Convey("Given a lock with a cell", t, func() {
		value := lock.New()
		So(value.Upsert(lock.Cell{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyNative}), ShouldBeNil)

		Convey("When the cell is looked up", func() {
			found, ok := value.Cell("acme/x", "claude", lock.ScopeUser)
			_, missing := value.Cell("acme/x", "codex", lock.ScopeUser)

			Convey("Then hits and misses are reported", func() {
				So(ok, ShouldBeTrue)
				So(found.Version, ShouldEqual, "1.0.0")
				So(missing, ShouldBeFalse)
			})
		})
	})
}

func TestUpsert(t *testing.T) {
	Convey("Given an empty lock", t, func() {
		value := lock.New()

		Convey("When cells are appended and replaced", func() {
			So(value.Upsert(lock.Cell{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyNative}), ShouldBeNil)
			So(value.Upsert(lock.Cell{Package: "acme/x", Host: "codex", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyLoose}), ShouldBeNil)
			So(value.Upsert(lock.Cell{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "2.0.0", Strategy: lock.StrategyNative}), ShouldBeNil)

			Convey("Then replace does not grow the slice", func() {
				So(value.Cells, ShouldHaveLength, 2)

				cell, ok := value.Cell("acme/x", "claude", lock.ScopeUser)
				So(ok, ShouldBeTrue)
				So(cell.Version, ShouldEqual, "2.0.0")
			})
		})

		Convey("When one of several cells is invalid", func() {
			good := lock.Cell{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1.0.0", Strategy: lock.StrategyNative}
			bad := lock.Cell{Package: "acme/y", Host: "claude", Scope: lock.ScopeUser, Strategy: lock.StrategySilenced}

			before := mustMarshal(t, value)

			err := value.Upsert(good, bad)

			Convey("Then nothing is applied", func() {
				So(err, ShouldBeError)
				So(value.Cells, ShouldBeEmpty)
				So(string(mustMarshal(t, value)), ShouldEqual, string(before))
			})
		})
	})
}

func TestUpsertPreservesExtras(t *testing.T) {
	Convey("Given a parsed lock whose cell carries unknown fields", t, func() {
		parsed, err := lock.Parse(readFixture(t, unknownPath))
		So(err, ShouldBeNil)

		Convey("When the same cell is upserted without extras", func() {
			err := parsed.Upsert(lock.Cell{Package: "acme/review-kit", Host: "claude", Scope: lock.ScopeUser, Version: "9.9.9", Strategy: lock.StrategyNative})
			So(err, ShouldBeNil)

			data := string(mustMarshal(t, parsed))

			Convey("Then the change lands and the unknown fields survive", func() {
				So(data, ShouldContainSubstring, `"version": "9.9.9"`)
				So(data, ShouldContainSubstring, "future_cell")
				So(data, ShouldContainSubstring, "future_cell_obj")
			})
		})

		Convey("When the cell returned by Cell is upserted back", func() {
			cell, ok := parsed.Cell("acme/review-kit", "claude", lock.ScopeUser)
			So(ok, ShouldBeTrue)

			cell.Version = "8.8.8"
			So(parsed.Upsert(cell), ShouldBeNil)

			data := string(mustMarshal(t, parsed))

			Convey("Then extras still survive", func() {
				So(data, ShouldContainSubstring, `"version": "8.8.8"`)
				So(data, ShouldContainSubstring, "future_cell")
			})
		})
	})
}

func TestDelete(t *testing.T) {
	Convey("Given a lock with three cells", t, func() {
		value := lock.New()
		So(value.Upsert(
			lock.Cell{Package: "acme/x", Host: "claude", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative},
			lock.Cell{Package: "acme/x", Host: "codex", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative},
			lock.Cell{Package: "acme/y", Host: "claude", Scope: lock.ScopeUser, Version: "1", Strategy: lock.StrategyNative},
		), ShouldBeNil)

		Convey("When cells are deleted", func() {
			removed := value.Delete("acme/x", "claude", lock.ScopeUser)
			again := value.Delete("acme/x", "claude", lock.ScopeUser)

			Convey("Then the count is honest and the rest is kept", func() {
				So(removed, ShouldEqual, 1)
				So(again, ShouldEqual, 0)
				So(value.Cells, ShouldHaveLength, 2)
			})
		})
	})
}

func TestSort(t *testing.T) {
	Convey("Given cells in arbitrary order", t, func() {
		value := lock.New()
		value.Cells = []lock.Cell{
			{Package: "b/x", Host: "claude", Scope: lock.ScopeUser},
			{Package: "a/x", Host: "z", Scope: lock.ScopeUser},
			{Package: "a/x", Host: "a", Scope: lock.ScopeUser},
			{Package: "a/x", Host: "a", Scope: lock.ScopeProject},
		}

		Convey("When they are sorted", func() {
			value.Sort()

			order := make([]string, 0, len(value.Cells))
			for _, cell := range value.Cells {
				order = append(order, cell.Package+"/"+cell.Host+"/"+cell.Scope)
			}

			Convey("Then the order is (package, host, scope)", func() {
				So(order, ShouldResemble, []string{
					"a/x/a/project",
					"a/x/a/user",
					"a/x/z/user",
					"b/x/claude/user",
				})
			})
		})
	})
}
