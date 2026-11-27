// Package web is the daemon's local web page (decision
// 2026-11-26-local-web-page): the embedded files of static/, index.html at /.
package web

import "net/http"

// Handler serves the page's files on 127.0.0.1:port, to the browser that
// holds the token of stateDir.
func Handler(stateDir string, port int) http.Handler {
	return guard(stateDir, port, http.FileServerFS(static))
}
