// The script every page shares. Loaded in <head>, so the stored theme is set
// before the page is drawn; a page's own script comes after it, deferred.

// el makes a tag of class cls (none: "") holding kids, nodes or strings. A
// string becomes text, never markup: data from the daemon goes in only so.
const el = (tag, cls, ...kids) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  n.append(...kids);
  return n;
};

// reachable shows whether the daemon answered: the top bar's state and, while
// it does not, a bar that says so. Call it once the page is parsed.
let lostBar;
const reachable = (ok) => {
  if (!lostBar) {
    lostBar = el("p", "attention warn", "Connection lost, retrying…");
    lostBar.setAttribute("role", "status");
    document.getElementById("main").prepend(lostBar);
  }
  lostBar.hidden = ok;
  const state = document.querySelector("[data-runner-state]");
  state.textContent = ok ? "Running" : "Not reachable";
  state.className = "status " + (ok ? "ok" : "error");
};

// listen calls onData with each event of the stream at url, parsed as JSON,
// and onOpen, if given, each time the stream (re)connects. While the stream is
// down a bar says so; the browser reconnects by itself, and after a refused
// answer listen tries again every 5 s. Call it once the page is parsed.
const listen = (url, onData, onOpen) => {
  const retry = () => setTimeout(open, 5000);
  const open = () => {
    const es = new EventSource(url);
    if (onOpen) es.onopen = onOpen;
    es.onmessage = (e) => { reachable(true); onData(JSON.parse(e.data)); };
    es.onerror = () => {
      reachable(false);
      if (es.readyState !== EventSource.CLOSED) return;
      // The browser gave up, after an answer other than 200. A 401 means the
      // token was replaced: reloading shows the daemon's own answer, the line
      // that names the command which lets this browser in again.
      fetch(url).then((r) => {
        r.body?.cancel();
        if (r.status === 401) location.reload();
        else retry();
      }, retry);
    };
  };
  open();
};

// bytes is n bytes in binary units, such as "8.5 GiB".
const bytes = (n) => {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  for (; n >= 1024 && i < units.length - 1; i++) n /= 1024;
  return (i ? n.toFixed(1) : n) + " " + units[i];
};

// ago is how long ago time t was, such as "3 minutes ago"; under a minute
// "just now", so the page does not tick every second.
const ago = (t) => {
  const s = (Date.now() - new Date(t)) / 1000;
  const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
  for (const [unit, size] of [["day", 86400], ["hour", 3600], ["minute", 60]]) {
    if (s >= size) return rtf.format(-Math.floor(s / size), unit);
  }
  return "just now";
};

// when is time t as "3 minutes ago", with the exact time on hover.
const when = (t) => {
  const n = el("time", "", ago(t));
  n.dateTime = t;
  n.title = new Date(t).toLocaleString();
  return n;
};

// runHref is the page of run r, an entry of /api/runs.
const runHref = (r) => "run.html?" + new URLSearchParams({ project: r.project, issue: r.issue, run: r.run });

(() => {
  // The light and dark switch (decision 2026-11-27-web-look). The page follows
  // the system until the switch is used; the choice is kept in this browser only.
  const root = document.documentElement;
  const system = matchMedia("(prefers-color-scheme: dark)");
  let stored = null;
  try { stored = localStorage.getItem("theme"); } catch { /* storage off: follow the system */ }
  if (stored === "light" || stored === "dark") root.dataset.theme = stored;

  const isDark = () => root.dataset.theme ? root.dataset.theme === "dark" : system.matches;

  // A link to a place inside a folded row opens that row. Chrome does this by
  // itself; Safari does not.
  const reveal = () => {
    const target = location.hash && document.getElementById(location.hash.slice(1));
    for (let d = target && target.closest("details"); d; d = d.parentElement.closest("details")) d.open = true;
    if (target) target.scrollIntoView();
  };

  document.addEventListener("DOMContentLoaded", () => {
    document.querySelectorAll(".topbar nav a").forEach((a) => {
      if (a.pathname === location.pathname) a.setAttribute("aria-current", "page");
    });

    const switches = document.querySelectorAll("[data-theme-switch]");
    const show = () => switches.forEach((s) => { s.checked = isDark(); });
    show();
    system.addEventListener("change", show);
    switches.forEach((s) => s.addEventListener("change", () => {
      root.dataset.theme = s.checked ? "dark" : "light";
      try { localStorage.setItem("theme", root.dataset.theme); } catch { /* kept for this page only */ }
      show();
    }));

    reveal();
    addEventListener("hashchange", reveal);
  });
})();
