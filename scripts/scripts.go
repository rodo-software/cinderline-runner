// Package scripts embeds the one-line installers, which `update` runs to
// download, verify and replace the binary.
package scripts

import _ "embed"

// InstallSh is install.sh, for macOS and Linux.
//
//go:embed install.sh
var InstallSh []byte

// InstallPs1 is install.ps1, for Windows.
//
//go:embed install.ps1
var InstallPs1 []byte
