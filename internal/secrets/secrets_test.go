package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderstack-runner/internal/config"
	"github.com/rodo-software/cinderstack-runner/internal/name"
)

var (
	projectA = config.Project{URL: "https://a.example.com", Key: "WEB"}
	projectB = config.Project{URL: "https://b.example.com", Key: "WEB"}
)

// The keyring mock replaces a global provider that cannot be restored, so
// this test must stay the first one in this file and no test may use t.Parallel.
func TestRealKeychain(t *testing.T) {
	user := fmt.Sprintf("test %d %d", os.Getpid(), time.Now().UnixNano())
	if err := keyring.Set(name.Name, user, "secret"); err != nil {
		t.Skipf("no usable keychain here: %v", err)
	}
	t.Cleanup(func() { keyring.Delete(name.Name, user) })
	got, err := keyring.Get(name.Name, user)
	if err != nil || got != "secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := keyring.Delete(name.Name, user); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(name.Name, user); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
}

func TestKeychainRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := New(t.TempDir())
	store, err := s.Set(projectA, RunnerToken, "tok")
	if err != nil || store != Keychain {
		t.Fatalf("Set = %q, %v", store, err)
	}
	v, store, err := s.Get(projectA, RunnerToken)
	if err != nil || v != "tok" || store != Keychain {
		t.Fatalf("Get = %q, %q, %v", v, store, err)
	}
	if err := s.Delete(projectA, RunnerToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(projectA, RunnerToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete(projectA, RunnerToken); err != nil {
		t.Fatalf("Delete of a missing secret: %v", err)
	}
}

func TestFallbackOnKeychainFailure(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	dir := t.TempDir()
	s := New(dir)
	store, err := s.Set(projectA, GitToken, "git")
	if err != nil || store != File {
		t.Fatalf("Set = %q, %v", store, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil || len(m) != 1 {
		t.Fatalf("file = %s, %v", data, err)
	}
	for _, v := range m {
		if v != "git" {
			t.Fatalf("file holds %q", v)
		}
	}
	v, store, err := s.Get(projectA, GitToken)
	if err != nil || v != "git" || store != File {
		t.Fatalf("Get = %q, %q, %v", v, store, err)
	}
	if _, _, err := s.Get(projectA, RunnerToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a missing secret with a failing keychain: %v, want ErrNotFound", err)
	}
	if err := s.Delete(projectA, GitToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(projectA, GitToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
}

func TestFileSecretFoundAfterKeychainRecovers(t *testing.T) {
	keyring.MockInitWithError(errors.New("locked"))
	s := New(t.TempDir())
	if store, err := s.Set(projectA, OpenRouterKey, "or"); err != nil || store != File {
		t.Fatalf("Set = %q, %v", store, err)
	}
	keyring.MockInit()
	v, store, err := s.Get(projectA, OpenRouterKey)
	if err != nil || v != "or" || store != File {
		t.Fatalf("Get = %q, %q, %v", v, store, err)
	}
	// A later Set that reaches the keychain removes the file copy.
	if store, err := s.Set(projectA, OpenRouterKey, "or2"); err != nil || store != Keychain {
		t.Fatalf("Set = %q, %v", store, err)
	}
	m, err := s.readFile()
	if err != nil || len(m) != 0 {
		t.Fatalf("file after keychain Set = %v, %v", m, err)
	}
}

func TestFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix modes")
	}
	keyring.MockInitWithError(errors.New("no secret service"))
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := New(dir).Set(projectA, RunnerToken, "tok"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "secrets.json"): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}

func TestProjectsWithSameKeyDoNotCollide(t *testing.T) {
	for _, fail := range []bool{false, true} {
		if fail {
			keyring.MockInitWithError(errors.New("no secret service"))
		} else {
			keyring.MockInit()
		}
		s := New(t.TempDir())
		if _, err := s.Set(projectA, RunnerToken, "a"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(projectB, RunnerToken); !errors.Is(err, ErrNotFound) {
			t.Fatalf("keychain failing=%v: B sees A's secret: %v", fail, err)
		}
		if _, err := s.Set(projectB, RunnerToken, "b"); err != nil {
			t.Fatal(err)
		}
		if v, _, _ := s.Get(projectA, RunnerToken); v != "a" {
			t.Fatalf("keychain failing=%v: A = %q, want a", fail, v)
		}
	}
}

func TestSizeLimit(t *testing.T) {
	keyring.MockInit()
	s := New(t.TempDir())
	if _, err := s.Set(projectA, AnthropicCredential, strings.Repeat("x", MaxSecretBytes)); err != nil {
		t.Fatalf("Set of %d bytes: %v", MaxSecretBytes, err)
	}
	_, err := s.Set(projectA, AnthropicCredential, strings.Repeat("x", MaxSecretBytes+1))
	if err == nil || !strings.Contains(err.Error(), "2560") || !strings.Contains(err.Error(), "2561") ||
		!strings.Contains(err.Error(), "Windows Credential Manager") || !strings.Contains(err.Error(), "WEB") {
		t.Fatalf("Set of %d bytes: %v", MaxSecretBytes+1, err)
	}
	if _, err := s.Set(projectA, AnthropicCredential, ""); err == nil {
		t.Fatal("Set of an empty value succeeded")
	}
}

func TestRepresentativeValuesFit(t *testing.T) {
	keyring.MockInit()
	s := New(t.TempDir())
	values := []struct {
		kind  Kind
		value string
	}{
		{AnthropicCredential, "sk-ant-api03-" + strings.Repeat("a", 108-len("sk-ant-api03-"))},
		{AnthropicCredential, "sk-ant-oat01-" + strings.Repeat("b", 400)},
		{OpenRouterKey, "sk-or-v1-" + strings.Repeat("c", 73-len("sk-or-v1-"))},
		{GitToken, strings.Repeat("d", 40)},
		{RunnerToken, strings.Repeat("e", 64)},
	}
	for _, c := range values {
		if _, err := s.Set(projectA, c.kind, c.value); err != nil {
			t.Fatalf("%s (%d bytes): %v", c.kind, len(c.value), err)
		}
		if v, _, err := s.Get(projectA, c.kind); err != nil || v != c.value {
			t.Fatalf("%s: Get = %q, %v", c.kind, v, err)
		}
	}
}

func TestCopyModelCredentials(t *testing.T) {
	keyring.MockInit()
	s := New(t.TempDir())
	for kind, v := range map[Kind]string{AnthropicCredential: "ant", OpenRouterKey: "or", RunnerToken: "run"} {
		if _, err := s.Set(projectA, kind, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CopyModelCredentials(projectA, projectB); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[Kind]string{AnthropicCredential: "ant", OpenRouterKey: "or"} {
		if v, _, err := s.Get(projectB, kind); err != nil || v != want {
			t.Errorf("B %s = %q, %v", kind, v, err)
		}
	}
	if _, _, err := s.Get(projectB, RunnerToken); !errors.Is(err, ErrNotFound) {
		t.Errorf("runner token copied: %v", err)
	}

	empty := config.Project{URL: "https://c.example.com", Key: "API"}
	if err := s.CopyModelCredentials(empty, projectB); !errors.Is(err, ErrNotFound) {
		t.Errorf("copy from a project without model credentials: %v, want ErrNotFound", err)
	}
}
