// Package defaultimage embeds the default image's Dockerfile, so the runner
// can build the image on a machine without this repository.
package defaultimage

import _ "embed"

// Dockerfile is images/default/Dockerfile.
//
//go:embed Dockerfile
var Dockerfile []byte
