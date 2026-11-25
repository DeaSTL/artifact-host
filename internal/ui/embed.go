package ui

import "embed"

// StaticFS embeds the compiled CSS, component JS bundle, host UI script, and artifact bridge shim.
//
//go:embed static
var StaticFS embed.FS
