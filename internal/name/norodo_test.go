package name

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRodoInstanceDefault guards EMBER-25: no code, script, prompt or the
// README points at Rodo's own instance or forge. Tests may use it as data.
func TestNoRodoInstanceDefault(t *testing.T) {
	root := filepath.Join("..", "..")
	paths := []string{filepath.Join(root, "README.md")}
	for _, dir := range []string{"cmd", "internal", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") {
				return err
			}
			paths = append(paths, p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), "rodo.nl") {
			t.Errorf("%s mentions rodo.nl", p)
		}
	}
}
