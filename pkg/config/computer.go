package config

// ComputerConfig enables the optional, Linux-only browser runtime on the
// workforce backend. Root is private execution storage, never a storage mount.
type ComputerConfig struct {
	Enabled    bool     `koanf:"enabled"`
	Root       string   `koanf:"root"`
	Node       string   `koanf:"node"`
	Chromium   string   `koanf:"chromium"`
	Playwright string   `koanf:"playwright"`
	IP         string   `koanf:"ip"`
	ReadPaths  []string `koanf:"read_paths"`
	AllowHosts []string `koanf:"allow_hosts"`
}
