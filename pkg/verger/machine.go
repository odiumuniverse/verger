package verger

// Machine is the resolved machine layout of one Client: the home, its state
// paths and the machine-global store. It is a pure value snapshot.
//
// Evolution rule: within a major version fields are only added, never renamed
// or removed, and consumers must ignore unknown JSON fields. HomeSource values
// are "env", "beadle-vault" or "default". Future path fields (consent, trust
// and receipts directories in T1.10/T1.12) arrive the same additive way.
type Machine struct {
	Home        string `json:"home"`
	HomeSource  string `json:"home_source"` // env|beadle-vault|default
	SpecPath    string `json:"spec_path"`
	LockPath    string `json:"lock_path"`
	StateDir    string `json:"state_dir"`
	JournalPath string `json:"journal_path"`
	StoreRoot   string `json:"store_root"`
	DataDir     string `json:"data_dir"`
	TrashDir    string `json:"trash_dir"`
	RuntimeDir  string `json:"runtime_dir"`
	BinDir      string `json:"bin_dir"`
	CacheDir    string `json:"cache_dir"`
}

// Machine returns the resolved paths without touching the filesystem.
func (c *Client) Machine() Machine {
	return Machine{
		Home:        c.home.Root(),
		HomeSource:  string(c.home.Source()),
		SpecPath:    c.home.SpecPath(),
		LockPath:    c.home.LockPath(),
		StateDir:    c.home.StateDir(),
		JournalPath: c.home.JournalPath(),
		StoreRoot:   c.store.Root(),
		DataDir:     c.store.DataDir(),
		TrashDir:    c.store.TrashDir(),
		RuntimeDir:  c.store.RuntimeDir(),
		BinDir:      c.store.BinDir(),
		CacheDir:    c.store.CacheDir(),
	}
}
