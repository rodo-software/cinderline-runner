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
// answer listen tries again every 5 s. The event named done ends the stream:
// the browser would otherwise connect again and get it all once more. listen
// returns the function that closes the stream. Call it once the page is parsed.
const listen = (url, onData, onOpen) => {
  let es, timer, closed = false;
  const retry = () => { if (!closed) timer = setTimeout(open, 5000); };
  const open = () => {
    es = new EventSource(url);
    if (onOpen) es.onopen = onOpen;
    es.onmessage = (e) => { reachable(true); onData(JSON.parse(e.data)); };
    es.addEventListener("done", () => es.close());
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
  return () => { closed = true; clearTimeout(timer); es.close(); };
};

// follower keeps the scrolling box at its newest content until the user
// scrolls up; then the button jump shows, which takes the box back down. It
// returns the function to call with each addition: keep(() => box.append(n)).
const follower = (box, jump) => {
  const atEnd = () => box.scrollTop + box.clientHeight >= box.scrollHeight - 8;
  const end = () => { box.scrollTop = box.scrollHeight; };
  box.addEventListener("scroll", () => { jump.hidden = atEnd(); });
  jump.addEventListener("click", () => { end(); box.focus(); });
  return (add) => {
    const stick = atEnd();
    add();
    if (stick) end();
  };
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

// took is a duration of ms milliseconds, such as "9 min".
const took = (ms) => {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + " s";
  const m = Math.floor(s / 60);
  return m < 60 ? m + " min" : Math.floor(m / 60) + " h " + (m % 60) + " min";
};

// outcome is the badge of a run's or a step's outcome, or of one in flight.
const outcome = (word, live) => live ? el("span", "badge running", "running")
  : el("span", "badge " + ({ ok: "ok", failed: "error", stopped: "warn" }[word] || ""), word || "none");

// projectState is a project's status dot, from its entry of /api/status and
// its count of runs in flight: the one fact that matters most.
const projectState = (p, runs) => {
  if (!p.connected) return p.last_error ? el("span", "status error", "Not connected") : el("span", "status", "Connecting");
  if (p.draining) return el("span", "status warn", "Draining");
  if (p.waits_for_memory) return el("span", "status warn", "Waits for memory");
  return runs ? el("span", "status running", "Running") : el("span", "status ok", "Idle");
};

// runCount is how many runs project p of /api/status has in flight, the
// Project Manager's among them.
const runCount = (p) => (p.issues || []).length + (p.manager ? 1 : 0);

// statuses is the entry of /api/status of each project of /api/config, or
// undefined while the daemon has none: status names a project by its label,
// and the nth project with a label gets the nth entry with it.
const statuses = (projects, st) => {
  const left = [...(st.projects || [])];
  return projects.map((p) => {
    const i = left.findIndex((s) => s && s.project === p.label);
    if (i < 0) return undefined;
    const s = left[i];
    left[i] = null;
    return s;
  });
};

// projectHref is the page of project p of /api/config: two projects can
// share a key, never a key, URL and workspace.
const projectHref = (p) => "project.html?" + new URLSearchParams({ key: p.key, url: p.url, workspace: p.workspace });

// added is the project of /api/config whose ID is not in ids, the IDs of
// before an add or a rename, or undefined: those routes answer 204 without
// naming it.
const added = (ids) => getJSON("/api/config").then((c) => (c.config.projects || []).filter((p) => !ids.includes(p.id)).pop());

// runHref is the page of run r, an entry of /api/runs.
const runHref = (r) => "run.html?" + new URLSearchParams({ project: r.project, issue: r.issue, run: r.run });

// getJSON is the JSON answer of url. A 401 reloads the page: the token was
// replaced, and the daemon's answer says what to do.
const getJSON = (url) => fetch(url).then((r) => {
  if (r.status === 401) location.reload();
  if (!r.ok) throw new Error(r.status + " " + r.statusText);
  return r.json();
});

// describe adds id to, or with on false removes it from, the
// aria-describedby of n, keeping the ids it holds besides.
const describe = (n, id, on) => {
  const ids = (n.getAttribute("aria-describedby") || "").split(" ").filter((x) => x && x !== id);
  if (on) ids.push(id);
  if (ids.length) n.setAttribute("aria-describedby", ids.join(" "));
  else n.removeAttribute("aria-describedby");
};

// send sends body as JSON with method to url, for button, which is disabled
// while it runs. A refusal shows the answer's error under button's form, or
// beside button when it has none, tied to its fields with aria-describedby;
// the next answer of 2xx takes it away. It resolves to whether the answer was
// 2xx: the page then loads its data again. The browser checks nothing
// itself: what the daemon refuses, it says.
const sendErrors = new WeakMap(); // a form's or a lone button's error line
let sendIds = 0;
const send = async (button, method, url, body) => {
  const at = button.form || button;
  const fields = button.form ? [...button.form.querySelectorAll("input, select, textarea")] : [button];
  let line = sendErrors.get(at);
  if (!line) {
    line = el("p", "field-error");
    line.id = "send-error-" + ++sendIds;
    line.setAttribute("role", "alert");
    sendErrors.set(at, line);
  }
  const show = (text) => {
    line.textContent = text;
    if (text) at.after(line);
    else line.remove();
    fields.forEach((f) => describe(f, line.id, !!text));
  };
  const focused = document.activeElement === button;
  button.disabled = true;
  try {
    const r = await fetch(url, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    if (r.status === 401) location.reload();
    if (r.ok) {
      show("");
      return true;
    }
    const answer = await r.json().catch(() => ({}));
    show(answer.error || r.status + " " + r.statusText);
  } catch {
    show("The runner did not answer.");
  } finally {
    button.disabled = false;
    // A disabled button loses the focus; it gets it back.
    if (focused && document.activeElement === document.body) button.focus();
  }
  return false;
};

// confirmed asks question in a modal <dialog> with the buttons Cancel and
// yes, Cancel focused, and resolves to whether yes was pressed; Escape
// cancels. The browser gives the focus back to where it was on closing.
const confirmed = (question, yes) => new Promise((resolve) => {
  const q = el("p", "", question);
  q.id = "confirm-question";
  const no = el("button", "", "Cancel");
  no.value = "no";
  no.autofocus = true;
  const ok = el("button", "danger", yes);
  ok.value = "yes";
  const form = el("form", "", q, el("div", "actions", no, ok));
  form.method = "dialog"; // a button closes the dialog with its value
  const d = el("dialog", "confirm", form);
  d.setAttribute("aria-labelledby", q.id);
  d.addEventListener("close", () => {
    d.remove();
    resolve(d.returnValue === "yes");
  });
  document.body.append(d);
  d.showModal();
});

// copyable is a block of the command text, to type in a terminal, with a
// button that copies it. Where the clipboard is refused the button selects
// the text instead, for Ctrl+C.
const copyable = (text) => {
  const code = el("code", "", text);
  const button = el("button", "quiet", "Copy");
  button.type = "button";
  button.setAttribute("aria-live", "polite"); // says "Copied" to a screen reader too
  const said = (word) => {
    button.textContent = word;
    setTimeout(() => { button.textContent = "Copy"; }, 2000);
  };
  button.addEventListener("click", () => {
    navigator.clipboard.writeText(text).then(() => said("Copied"), () => {
      getSelection().selectAllChildren(code);
      said("Press Ctrl+C");
    });
  });
  return el("div", "copy", el("pre", "", code), button);
};

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
