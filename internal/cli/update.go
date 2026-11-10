package cli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

func (c *CLI) update(args []string) int {
	f := c.flags("update", "[--refresh]")
	refresh := f.Bool("refresh", false, "refresh the tools volume and default image")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return 2
	}
	if *refresh {
		return c.updateRefresh()
	}
	return c.updateMain()
}

func (c *CLI) updateRefresh() int {
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}

	if _, _, err := c.agentImage(ctx, "", cfg.BuildImage); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.Stdout, "Refreshed the tools volume and the default image")
	return 0
}

func (c *CLI) updateMain() int {
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	exePath := c.exe
	if exePath == "" {
		var err error
		exePath, err = os.Executable()
		if err != nil {
			return c.fail(fmt.Errorf("resolve exe: %w", err))
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			return c.fail(fmt.Errorf("resolve exe: %w", err))
		}
	}

	if runtime.GOOS == "windows" {
		os.Remove(exePath + ".old")
	}

	base := os.Getenv(strings.ToUpper(name.Name) + "_DOWNLOAD_URL")
	if base == "" {
		base = "https://github.com/rodo-software/" + name.Name + "-runner/releases/latest/download"
	}

	asset := name.Name + "_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		asset += ".zip"
	} else {
		asset += ".tar.gz"
	}

	dlDir, err := os.MkdirTemp("", name.Name+"-download-")
	if err != nil {
		return c.fail(fmt.Errorf("download: %w", err))
	}
	defer os.RemoveAll(dlDir)

	archivePath := filepath.Join(dlDir, asset)
	if err := downloadFile(ctx, base+"/"+asset, archivePath); err != nil {
		return c.fail(fmt.Errorf("download: %w", err))
	}

	checksumsPath := filepath.Join(dlDir, "checksums.txt")
	if err := downloadFile(ctx, base+"/checksums.txt", checksumsPath); err != nil {
		return c.fail(fmt.Errorf("download: %w", err))
	}

	checksumsData, err := os.ReadFile(checksumsPath)
	if err != nil {
		return c.fail(fmt.Errorf("checksum: %w", err))
	}

	var expectedSum string
	for _, line := range strings.Split(strings.TrimSpace(string(checksumsData)), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == asset {
			expectedSum = parts[0]
			break
		}
	}
	if expectedSum == "" {
		return c.fail(fmt.Errorf("checksum: no checksum found for %s", asset))
	}

	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		return c.fail(fmt.Errorf("checksum: %w", err))
	}
	h := sha256.Sum256(archiveData)
	actualSum := hex.EncodeToString(h[:])
	if actualSum != expectedSum {
		return c.fail(fmt.Errorf("checksum: mismatch for %s (expected %s, got %s)", asset, expectedSum, actualSum))
	}

	binaryName := name.Name
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(exePath), "."+name.Name+"-update-")
	if err != nil {
		return c.fail(fmt.Errorf("extract: %w", err))
	}
	defer os.RemoveAll(tmpDir)

	nextPath := filepath.Join(tmpDir, binaryName)

	if runtime.GOOS == "windows" {
		if err := extractFromZip(archivePath, binaryName, nextPath); err != nil {
			return c.fail(fmt.Errorf("extract: %w", err))
		}
	} else {
		if err := extractFromTarGz(archivePath, binaryName, nextPath); err != nil {
			return c.fail(fmt.Errorf("extract: %w", err))
		}
	}
	if err := os.Chmod(nextPath, 0o755); err != nil {
		return c.fail(fmt.Errorf("extract: %w", err))
	}

	x := c.exec
	if x == nil {
		x = workspace.RunCommand
	}

	out, err := x(ctx, nil, nextPath, "version")
	if err != nil {
		return c.fail(fmt.Errorf("version: %w", err))
	}
	parts := strings.Fields(out)
	if len(parts) != 2 || parts[0] != name.Name {
		return c.fail(fmt.Errorf("version: the new binary answered %q", out))
	}
	newVersion := parts[1]
	if newVersion == c.Version {
		fmt.Fprintf(c.Stdout, "Already at %s\n", c.Version)
		return 0
	}

	if _, err := x(ctx, nil, nextPath, "update", "--refresh"); err != nil {
		return c.fail(fmt.Errorf("update --refresh: %w", err))
	}

	oldVersion := c.Version
	if err := replaceExe(exePath, nextPath); err != nil {
		return c.fail(fmt.Errorf("replace: %w", err))
	}

	drainErr := c.drainAndWait(ctx)
	if drainErr != nil && !errors.Is(drainErr, localapi.ErrNotRunning) {
		fmt.Fprintf(c.Stderr, "Updated to %s, but the drain failed: %v\n", newVersion, drainErr)
		return 1
	}

	fmt.Fprintf(c.Stdout, "Updated %s from %s to %s\n", name.Name, oldVersion, newVersion)
	if drainErr == nil {
		start := c.startService
		if start == nil {
			start = startService
		}
		installed, err := start(exePath)
		if err != nil {
			fmt.Fprintf(c.Stderr, "error: updated to %s, but starting the service failed: %v\n", newVersion, err)
			return 1
		}
		if installed {
			fmt.Fprintf(c.Stdout, "Started the %s service with %s\n", name.Name, newVersion)
		} else {
			fmt.Fprintf(c.Stdout, "The running %s finished its runs and stopped; start it again with `%s run`\n", name.Name, name.Name)
		}
	}
	return 0
}

func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

func extractFromTarGz(archivePath, entry, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("no %s in archive", entry)
		}
		if err != nil {
			return err
		}
		if hdr.Name != entry || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
		return err
	}
}

func extractFromZip(archivePath, entry, dst string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if zf.Name != entry {
			continue
		}
		in, err := zf.Open()
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
		return err
	}
	return fmt.Errorf("no %s in archive", entry)
}
