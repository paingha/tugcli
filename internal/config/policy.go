package config

// PolicyConfig is policy in config.yaml: the policy packs tug check runs
// besides the built-in baseline pack.
type PolicyConfig struct {
	// Packs to load, from files or from URLs pinned by their sha256.
	Packs []PolicyPackSource `json:"packs,omitempty"`
	// Default names the packs tug check runs without --pack. Default:
	// every pack, the baseline included.
	Default []string `json:"default,omitempty"`
}

// PolicyPackSource is where one policy pack comes from: a file, or an https
// URL whose content must have the given sha256.
type PolicyPackSource struct {
	// Path is the pack file; a relative path is taken from the config
	// directory.
	Path string `json:"path,omitempty"`
	// URL is the pack to download over https.
	URL string `json:"url,omitempty"`
	// SHA256 pins the pack's content. URLs need one; for a path it is
	// checked when given.
	SHA256 string `json:"sha256,omitempty"`
}
