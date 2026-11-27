package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// tokenFile is the token's file in the state folder.
const tokenFile = "web-token"

// Token is the page's token from stateDir's web-token, made there (32 bytes
// from crypto/rand as hex, mode 0600) when missing.
func Token(stateDir string) (string, error) {
	path := filepath.Join(stateDir, tokenFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := makeToken(stateDir); err != nil {
			return "", fmt.Errorf("make the web token: %w", err)
		}
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	// An empty or cut-off file must never let anyone in.
	if _, err := hex.DecodeString(tok); err != nil || len(tok) != 64 {
		return "", fmt.Errorf("%s is not a token of 64 hex characters", path)
	}
	return tok, nil
}

// makeToken writes a new token to stateDir's web-token unless one is there:
// linked, not renamed, so two at once never replace each other's.
func makeToken(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir, tokenFile+".tmp-*") // made 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	b := make([]byte, 32)
	rand.Read(b) // never fails (Go 1.24)
	_, err = f.WriteString(hex.EncodeToString(b))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Link(f.Name(), filepath.Join(stateDir, tokenFile)); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// guard lets only the user's own browser through to h (decision
// 2026-11-26-local-web-page, section Access). It reads the token on every
// request, so a replaced token file takes effect without a restart.
func guard(stateDir string, port int, h http.Handler) http.Handler {
	p := ":" + strconv.Itoa(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		// DNS rebinding: a foreign name that resolves to 127.0.0.1.
		if r.Host != "127.0.0.1"+p && r.Host != "localhost"+p {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		tok, err := Token(stateDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		right := func(s string) bool { return subtle.ConstantTimeCompare([]byte(s), []byte(tok)) == 1 }
		if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Has("token") {
			if !right(r.URL.Query().Get("token")) {
				unauthorized(w)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: name.Name, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(name.Name); err != nil || !right(c.Value) {
			unauthorized(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get("Origin") != "http://"+r.Host && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "Open this page with the command: "+name.Name+" web", http.StatusUnauthorized)
}
