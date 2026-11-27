package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// static is the page's files, at the root.
var static, _ = fs.Sub(embedded, "static")
