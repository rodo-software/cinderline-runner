// Package proxyimage embeds the egress proxy's Dockerfile and source, so the
// runner can build its image on a machine without this repository.
package proxyimage

import _ "embed"

// Dockerfile is images/proxy/Dockerfile.
//
//go:embed Dockerfile
var Dockerfile []byte

// Main is images/proxy/cmd/main.go, the proxy's only source file.
//
//go:embed cmd/main.go
var Main []byte
