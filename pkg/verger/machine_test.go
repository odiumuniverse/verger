package verger

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestMachineSnapshot(t *testing.T) {
	Convey("Given an open client", t, func() {
		homeRoot := filepath.Join(t.TempDir(), "home")
		storeRoot := filepath.Join(t.TempDir(), "store")

		c, err := Open(context.Background(), WithHome(homeRoot), WithStore(storeRoot))
		So(err, ShouldBeNil)

		defer func() { _ = c.Close() }()

		state := filepath.Join(homeRoot, "state")

		Convey("When Machine is read", func() {
			m := c.Machine()

			Convey("Then every field is a resolved absolute path", func() {
				So(m.Home, ShouldEqual, homeRoot)
				So(m.HomeSource, ShouldEqual, "env")
				So(m.SpecPath, ShouldEqual, filepath.Join(homeRoot, "verger.toml"))
				So(m.LockPath, ShouldEqual, filepath.Join(homeRoot, "verger.lock"))
				So(m.StateDir, ShouldEqual, state)
				So(m.JournalPath, ShouldEqual, filepath.Join(state, "journal.jsonl"))
				So(m.StoreRoot, ShouldEqual, storeRoot)
				So(m.DataDir, ShouldEqual, filepath.Join(storeRoot, "data"))
				So(m.TrashDir, ShouldEqual, filepath.Join(storeRoot, "trash"))
				So(m.RuntimeDir, ShouldEqual, filepath.Join(storeRoot, "runtime"))
				So(m.BinDir, ShouldEqual, filepath.Join(storeRoot, "bin"))
				So(m.CacheDir, ShouldEqual, filepath.Join(storeRoot, "cache"))

				So(filepath.IsAbs(m.Home), ShouldBeTrue)
				So(filepath.IsAbs(m.StoreRoot), ShouldBeTrue)
			})

			Convey("Then it is a value snapshot that does not change with later calls", func() {
				m.Home = "mutated"

				So(c.Machine().Home, ShouldEqual, homeRoot)
			})
		})

		Convey("When Machine is marshaled", func() {
			data, marshalErr := json.Marshal(c.Machine())

			Convey("Then the JSON keys are the documented snake_case ones", func() {
				So(marshalErr, ShouldBeNil)

				var doc map[string]string
				So(json.Unmarshal(data, &doc), ShouldBeNil)
				So(doc["home"], ShouldEqual, homeRoot)
				So(doc["home_source"], ShouldEqual, "env")
				So(doc["spec_path"], ShouldEqual, filepath.Join(homeRoot, "verger.toml"))
				So(doc["lock_path"], ShouldEqual, filepath.Join(homeRoot, "verger.lock"))
				So(doc["state_dir"], ShouldEqual, state)
				So(doc["journal_path"], ShouldEqual, filepath.Join(state, "journal.jsonl"))
				So(doc["store_root"], ShouldEqual, storeRoot)
				So(doc["data_dir"], ShouldEqual, filepath.Join(storeRoot, "data"))
				So(doc["trash_dir"], ShouldEqual, filepath.Join(storeRoot, "trash"))
				So(doc["runtime_dir"], ShouldEqual, filepath.Join(storeRoot, "runtime"))
				So(doc["bin_dir"], ShouldEqual, filepath.Join(storeRoot, "bin"))
				So(doc["cache_dir"], ShouldEqual, filepath.Join(storeRoot, "cache"))
			})
		})
	})
}
