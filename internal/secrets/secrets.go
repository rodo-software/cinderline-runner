// Package secrets holds the per-project secrets on this machine: the OS
// keychain through go-keyring, with a 0600 file in the state folder where
// the keychain fails (decision 2026-11-05-go-keyring). Nothing else reads secrets.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// Kind is the kind of secret a project holds.
type Kind string

const (
	RunnerToken Kind = "runner-token"
	GitToken    Kind = "git-token"
	// AnthropicCredential is an API key or a `claude setup-token` token; the package does not tell them apart.
	AnthropicCredential Kind = "anthropic"
	OpenRouterKey       Kind = "openrouter"
	// OpenAIKey is an OpenAI API key, for Codex.
	OpenAIKey Kind = "openai"
)

// modelCredentials are the kinds CopyModelCredentials copies.
var modelCredentials = []Kind{AnthropicCredential, OpenRouterKey, OpenAIKey}

// Store says where a secret was found or written.
type Store string

const (
	Keychain Store = "keychain"
	File     Store = "file"
)

// MaxSecretBytes is the Windows Credential Manager cap on a secret. It holds on
// every OS so a secret that works on one machine works on all.
const MaxSecretBytes = 2560

// ErrNotFound means neither the keychain nor the fallback file holds the secret.
var ErrNotFound = errors.New("secret not found")

// Secrets reads and writes secrets. Safe for concurrent use within one process.
type Secrets struct {
	file string
	mu   sync.Mutex // guards the read-modify-write of file
}

// New returns the secrets of this machine with the fallback file in stateDir.
func New(stateDir string) *Secrets {
	return &Secrets{file: filepath.Join(stateDir, "secrets.json")}
}

// entry is the keychain user and the file key. A project key has no
// whitespace and a kind no space, so the URL last keeps it unambiguous.
func entry(p config.Project, k Kind) string {
	return string(k) + " " + p.Key + " " + p.URL
}

// Get returns the secret and the store that held it. A failing keychain is
// treated as not holding it: a locked or absent Secret Service is normal on headless Linux.
func (s *Secrets) Get(p config.Project, k Kind) (string, Store, error) {
	if v, err := keyring.Get(name.Name, entry(p, k)); err == nil {
		return v, Keychain, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readFile()
	if err != nil {
		return "", "", err
	}
	if v, ok := m[entry(p, k)]; ok {
		return v, File, nil
	}
	return "", "", fmt.Errorf("%s of project %s at %s: %w", k, p.Key, p.URL, ErrNotFound)
}

// Set stores the secret in the keychain, or in the fallback file when the keychain fails.
func (s *Secrets) Set(p config.Project, k Kind, value string) (Store, error) {
	if value == "" {
		return "", fmt.Errorf("%s of project %s at %s: empty value", k, p.Key, p.URL)
	}
	if len(value) > MaxSecretBytes {
		return "", fmt.Errorf("%s of project %s at %s is %d bytes; the limit is %d, the Windows Credential Manager cap, on every OS",
			k, p.Key, p.URL, len(value), MaxSecretBytes)
	}
	e := entry(p, k)
	kerr := keyring.Set(name.Name, e, value)
	s.mu.Lock()
	defer s.mu.Unlock()
	if kerr == nil {
		// Remove an older file copy so a later keychain failure cannot surface a stale value.
		if err := s.updateFile(func(m map[string]string) bool { _, ok := m[e]; delete(m, e); return ok }); err != nil {
			return Keychain, fmt.Errorf("stored %s in the keychain but could not remove its old copy from %s: %w", k, s.file, err)
		}
		return Keychain, nil
	}
	if err := s.updateFile(func(m map[string]string) bool { m[e] = value; return true }); err != nil {
		return "", fmt.Errorf("storing %s of project %s at %s: keychain: %v; fallback file: %w", k, p.Key, p.URL, kerr, err)
	}
	return File, nil
}

// Delete removes the secret from both stores. A missing secret is not an
// error; keychain errors are treated as the keychain not holding it.
func (s *Secrets) Delete(p config.Project, k Kind) error {
	e := entry(p, k)
	keyring.Delete(name.Name, e)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateFile(func(m map[string]string) bool { _, ok := m[e]; delete(m, e); return ok })
}

// CopyModelCredentials sets on to every model credential from holds. Runner
// and git tokens are never copied. It returns ErrNotFound when from has none.
func (s *Secrets) CopyModelCredentials(from, to config.Project) error {
	copied := false
	for _, k := range modelCredentials {
		v, _, err := s.Get(from, k)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := s.Set(to, k, v); err != nil {
			return err
		}
		copied = true
	}
	if !copied {
		return fmt.Errorf("no model credential on project %s at %s: %w", from.Key, from.URL, ErrNotFound)
	}
	return nil
}

// readFile returns the fallback file's entries; a missing file is empty. Callers hold mu.
func (s *Secrets) readFile() (map[string]string, error) {
	data, err := os.ReadFile(s.file)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", s.file, err)
	}
	return m, nil
}

// updateFile applies change to the fallback file and writes it back when change
// reports a change, atomically like config.Save. Callers hold mu.
func (s *Secrets) updateFile(change func(map[string]string) bool) (err error) {
	m, err := s.readFile()
	if err != nil || !change(m) {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".secrets-*.json") // created 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.file)
}
