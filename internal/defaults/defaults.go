// Package defaults holds the embedded default configuration (defaults.toml),
// the base layer under every dashy.toml.
package defaults

import _ "embed"

//go:embed defaults.toml
var Config string
