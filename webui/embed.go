// Package webui embeds the static management interface.
package webui

import (
	"embed"
)

// Assets contains the static management interface served by the admin listener.
//
//go:embed index.html css js
var Assets embed.FS
