package environment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	defaultimage "github.com/rodo-software/cinderline-runner/images/default"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// pin sets the published pin for one test.
func pin(t *testing.T, digest, sum string) {
	t.Helper()
	oldDigest, oldSum := publishedDigest, publishedSum
	publishedDigest, publishedSum = digest, sum
	t.Cleanup(func() { publishedDigest, publishedSum = oldDigest, oldSum })
}

// dockerfileSum is the 12 hex of the embedded Dockerfile, as the local tag uses.
func dockerfileSum() string {
	s := sha256.Sum256(defaultimage.Dockerfile)
	return hex.EncodeToString(s[:6])
}

func pinnedRef() string { return PublishedImage + "@" + testDigest }

func localTag() string { return name.Name + "-default:" + dockerfileSum() }

func inspect(ref string) []string {
	return []string{"docker", "image", "inspect", "--format", "{{.Id}}", ref}
}

// CSR-49: the image name is built from internal/name, on the GHCR org of the module.
func TestPublishedImageName(t *testing.T) {
	if want := "ghcr.io/rodo-software/" + name.Name + "-runner-default"; PublishedImage != want {
		t.Errorf("PublishedImage = %q, want %q", PublishedImage, want)
	}
}

// CSR-49: a filled-in pin is a digest and the 12 hex of the Dockerfile it was built from.
func TestPinFormat(t *testing.T) {
	if publishedDigest == "" && publishedSum == "" {
		return
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(publishedDigest) || !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(publishedSum) {
		t.Errorf("pin = %q, %q; want sha256:<64 hex> and <12 hex>", publishedDigest, publishedSum)
	}
}

// CSR-49: a pinned image already on the machine is used as is.
func TestImagePinnedPresent(t *testing.T) {
	pin(t, testDigest, dockerfileSum())
	d, calls := scripted(nil, nil)
	ref, err := d.Image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ref != pinnedRef() {
		t.Errorf("image = %q, want %q", ref, pinnedRef())
	}
	if want := []execCall{{nil, inspect(pinnedRef())}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

// CSR-49: a missing pinned image is pulled by its digest.
func TestImagePulledByPinnedDigest(t *testing.T) {
	pin(t, testDigest, dockerfileSum())
	d, calls := scripted(nil, map[int]error{0: errors.New("no such image")})
	ref, err := d.Image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ref != pinnedRef() {
		t.Errorf("image = %q, want %q", ref, pinnedRef())
	}
	want := []execCall{{nil, inspect(pinnedRef())}, {nil, []string{"docker", "pull", "--quiet", pinnedRef()}}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

// CLR-240: a failed pull of the pinned image is an error, not a local build
// from unpinned inputs: the run does not start and the next poll tries again.
// CSR-151: the slow step is announced once.
func TestImagePullFailureIsError(t *testing.T) {
	pin(t, testDigest, dockerfileSum())
	d, calls := scripted(nil, map[int]error{0: errors.New("no such image"), 1: errors.New("offline")})
	fetches := 0
	d.OnFetch = func() { fetches++ }
	ref, err := d.Image(context.Background())
	if err == nil || !strings.Contains(err.Error(), "offline") || !strings.Contains(err.Error(), pinnedRef()) {
		t.Fatalf("Image = %q, %v; want the pull's error naming the image", ref, err)
	}
	if fetches != 1 {
		t.Errorf("OnFetch called %d times, want 1", fetches)
	}
	want := []execCall{{nil, inspect(pinnedRef())}, {nil, []string{"docker", "pull", "--quiet", pinnedRef()}}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

// localOnly checks d.Image skips GHCR: the local tag and only the local inspect.
func localOnly(t *testing.T, d *Docker, calls *[]execCall) {
	t.Helper()
	ref, err := d.Image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ref != localTag() {
		t.Errorf("image = %q, want %q", ref, localTag())
	}
	if want := []execCall{{nil, inspect(localTag())}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

// CSR-49: a Dockerfile changed since the pinned image was published is built locally.
func TestImageBuiltWhenDockerfileChanged(t *testing.T) {
	pin(t, testDigest, "000000000000")
	d, calls := scripted(nil, nil)
	localOnly(t, d, calls)
}

// CSR-49: no pin yet (nothing published) means the local build.
func TestImageBuiltWhenNotPinned(t *testing.T) {
	pin(t, "", "")
	d, calls := scripted(nil, nil)
	localOnly(t, d, calls)
}

// CSR-49: the workflow publishes the image the runner pulls. CLR-240: a pin
// for the current Dockerfile needs an amd64 image too, as amd64 machines no
// longer build when the pull fails (decision 2026-11-24-default-image-amd64).
func TestWorkflowDraft(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "default-image.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{PublishedImage, "linux/arm64", "images/default", "attest"}
	if publishedDigest != "" && publishedSum == dockerfileSum() {
		want = append(want, "linux/amd64")
	}
	for _, s := range want {
		if !strings.Contains(string(b), s) {
			t.Errorf("default-image.yml lacks %q", s)
		}
	}
}

// CLR-240: the draft that replaces the workflow publishes both platforms as
// one manifest list and attests it.
func TestWorkflowDraftMultiArch(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "ci", "default-image.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{PublishedImage, "linux/arm64", "linux/amd64", "imagetools create", "images/default", "attest"} {
		if !strings.Contains(string(b), s) {
			t.Errorf("docs/ci/default-image.yml lacks %q", s)
		}
	}
}
