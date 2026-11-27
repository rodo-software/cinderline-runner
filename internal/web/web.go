// Package web is the daemon's local web page (decision
// 2026-11-26-local-web-page): the embedded files of static/, index.html at /.
package web

import "net/http"

// Handler serves the page's files.
func Handler() http.Handler {
	return http.FileServerFS(static)
}
