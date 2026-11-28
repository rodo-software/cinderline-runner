package manage

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// fakeUpdate is an Exec whose install step succeeds without running and
// whose new binary answers version with answer and update --refresh with
// refreshErr; it records each command for the binary.
func fakeUpdate(answer string, refreshErr error) (func(context.Context, []string, ...string) (string, error), *[]string) {
	var ran []string
	return func(_ context.Context, _ []string, argv ...string) (string, error) {
		if argv[0] != "/bin/"+name.Name {
			return "", nil // the install script
		}
		ran = append(ran, strings.Join(argv[1:], " "))
		if argv[1] == "version" {
			return answer, nil
		}
		return "", refreshErr
	}, &ran
}

// CLR-303: Update installs, asks the new binary its version and has it
// refresh the tools; when nothing changed it refreshes nothing.
func TestUpdate(t *testing.T) {
	for _, tc := range []struct {
		answer, want string
		ran          []string
	}{
		{name.Name + " 9.9.9\n", "9.9.9", []string{"version", "update --refresh"}},
		{name.Name + " 1.2.3\n", "1.2.3", []string{"version"}},
	} {
		x, ran := fakeUpdate(tc.answer, nil)
		got, err := Update(context.Background(), x, "/bin/"+name.Name, "1.2.3")
		if err != nil || got != tc.want || !slices.Equal(*ran, tc.ran) {
			t.Errorf("answer %q: %q, %v, ran %q; want %q and %q", tc.answer, got, err, *ran, tc.want, tc.ran)
		}
	}
}

func TestUpdateFails(t *testing.T) {
	x, _ := fakeUpdate("something else", nil)
	if _, err := Update(context.Background(), x, "/bin/"+name.Name, "1.2.3"); err == nil || !strings.Contains(err.Error(), "something else") {
		t.Errorf("a strange version answer: %v, want an error naming it", err)
	}
	x, _ = fakeUpdate(name.Name+" 9.9.9", errors.New("docker build failed"))
	if _, err := Update(context.Background(), x, "/bin/"+name.Name, "1.2.3"); err == nil || !strings.Contains(err.Error(), "docker build failed") {
		t.Errorf("a failed refresh: %v, want its error", err)
	}
}
