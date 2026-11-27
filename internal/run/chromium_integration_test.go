//go:build integration

package run

// CLR-319 on real Docker: the default image's Chromium screenshots a local
// page as uid 1000 in the hardened agent container, by itself and through
// Playwright launched with executablePath, and asks the egress proxy for no
// host but the npm registry Playwright installs from. The integration CI job
// runs it with -run Integration ./internal/run/.

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// chromiumScript prints "chromium: png" and "playwright: png" when each
// wrote a PNG. Playwright is its latest release, the one an agent installs.
const chromiumScript = `set -e
chromium --version
cd /tmp
printf '<!doctype html><title>t</title><h1>Hello &#x1F44B;</h1>' > page.html
chromium --headless --disable-gpu --screenshot=/tmp/s.png --window-size=800,600 file:///tmp/page.html
[ "$(head -c 8 s.png | od -An -tx1 | tr -d ' \n')" = 89504e470d0a1a0a ] && echo "chromium: png"
mkdir pw && cd pw
npm install --no-audit --no-fund --loglevel=error playwright
cat > shot.js <<'JS'
const { chromium } = require('playwright');
(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH });
  const page = await browser.newPage();
  await page.goto('file:///tmp/page.html');
  const png = await page.screenshot();
  await browser.close();
  if (png.subarray(0, 8).toString('hex') === '89504e470d0a1a0a') console.log('playwright: png');
})().catch((e) => { console.error(e); process.exit(1); });
JS
node shot.js
`

func TestIntegrationChromium(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var logs lockedBuffer
	project := "ITC" + strconv.Itoa(os.Getpid())
	var (
		mu      sync.Mutex
		refused []string
	)
	env := &environment.Docker{
		Project: project,
		Exec:    workspace.RunCommand,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
		Allow:   []string{"registry.npmjs.org:443"},
		Memory:  2 << 30,
		OnRefused: func(_ string, r environment.Refusal) {
			mu.Lock()
			defer mu.Unlock()
			refused = append(refused, r.Host+" "+r.Reason)
		},
	}
	t.Cleanup(func() {
		if err := env.Cleanup(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		t.Log(logs.String())
	})

	out, err := env.Run(ctx, environment.Spec{
		Issue:   project + "-1",
		Dir:     "/tmp",
		Cmd:     []string{"sh", "-c", chromiumScript},
		Timeout: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run = %v\n%s", err, out)
	}
	for _, want := range []string{"chromium: png", "playwright: png"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("agent output lacks %q:\n%s", want, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refused) > 0 {
		t.Errorf("proxy refused %q; a render must ask for no host", refused)
	}
}
