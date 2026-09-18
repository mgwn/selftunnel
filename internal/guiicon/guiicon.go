// Package guiicon embeds the app icon so both GUI binaries can set it on
// the window without depending on files on disk.
package guiicon

import (
	_ "embed"

	"fyne.io/fyne/v2"
)

//go:embed icon.png
var iconPNG []byte

// Resource is the selftunnel icon (derived from assets/logo.svg).
var Resource = fyne.NewStaticResource("icon.png", iconPNG)
