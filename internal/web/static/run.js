// One run: its header and steps from /api/runs/{project}/{issue}/{run}, read
// again every 3 s while the run is live, and for each open step its events.
// Every text here comes from an agent or an issue: it goes in as text only.
(() => {
  const $ = (id) => document.getElementById(id);
  const q = new URLSearchParams(location.search);
  const [project, issue, run] = ["project", "issue", "run"].map((k) => q.get(k) || "");
  const base = "/api/runs/" + [project, issue, run].map(encodeURIComponent).join("/");
  const maxLines = 20000; // out lines of one step kept in the page

  const missing = (text) => {
    $("missing").textContent = text;
    $("missing").hidden = false;
    $("steps-section").hidden = true;
  };

  const head = (r) => {
    document.title = r.issue + " · Runs · Cinderline runner";
    $("project").textContent = r.project;
    $("project").href = "runs.html?" + new URLSearchParams({ project: r.project });
    $("issue").textContent = r.issue;
    const lead = [outcome(r.outcome, r.live), el("span", "", "started ", when(r.start)),
      el("span", "", took(new Date(r.end || Date.now()) - new Date(r.start)))];
    if (r.manager) lead.push(el("span", "badge", "Project Manager"));
    $("lead").replaceChildren(...lead);
    $("lead").hidden = false;
    $("cinderline").textContent = "Open " + r.issue + " in Cinderline ↗";
    $("cinderline").href = r.issue_url;
    $("links").hidden = !r.issue_url;
  };

  // view is what an open step shows of its events stream, filled as they come.
  const view = () => {
    const text = el("pre", "box");
    const prompt = el("details", "fold", el("summary", "", "Prompt"), text);
    const hosts = el("tbody");
    const cols = ["Host", "Reason", "Method", "URL"].map((h) => {
      const th = el("th", "", h);
      th.scope = "col";
      return th;
    });
    const refused = el("div", "", el("h3", "", "Hosts refused"),
      el("div", "scroll", el("table", "rows", el("thead", "", el("tr", "", ...cols)), hosts)));
    const earlier = el("p", "muted small");
    const out = el("pre", "box");
    out.tabIndex = 0;
    out.setAttribute("role", "region");
    out.setAttribute("aria-label", "Output lines");
    for (const n of [prompt, refused, earlier, out]) n.hidden = true;
    return { root: el("div", "", prompt, refused, earlier, out), text, prompt, hosts, refused, earlier, out, pending: [], dropped: 0, frame: 0 };
  };

  const atBottom = (n) => n.scrollTop + n.clientHeight >= n.scrollHeight - 8;

  // flush draws a step's out lines that came since the last frame: a finished
  // step sends all its lines at once, and the box is laid out once a frame.
  const flush = (v) => {
    v.frame = 0;
    const stick = atBottom(v.out);
    const lines = v.pending.slice(-maxLines);
    v.dropped += v.pending.length - lines.length;
    v.pending = [];
    const f = document.createDocumentFragment();
    for (const line of lines) f.append(el("span", "", line + "\n")); // a span a line, to drop the oldest
    v.out.append(f);
    for (; v.out.childElementCount > maxLines; v.dropped++) v.out.firstElementChild.remove();
    v.earlier.textContent = v.dropped + (v.dropped === 1 ? " earlier line" : " earlier lines") + " not shown";
    v.earlier.hidden = !v.dropped;
    v.out.hidden = false;
    if (stick) v.out.scrollTop = v.out.scrollHeight;
  };

  const add = (v, ev) => {
    if (ev.type === "prompt") {
      v.text.textContent = ev.text ?? "";
      v.prompt.hidden = false;
    } else if (ev.type === "refused") {
      v.hosts.append(el("tr", "", ...[ev.host, ev.reason, ev.method, ev.url].map((s) => el("td", "", s ?? ""))));
      v.refused.hidden = false;
    } else if (ev.type === "out") {
      // A hidden tab draws no frames: drop the oldest half once twice the kept lines wait.
      if (v.pending.push(ev.line ?? "") >= 2 * maxLines) v.dropped += v.pending.splice(0, maxLines).length;
      v.frame ||= requestAnimationFrame(() => flush(v));
    }
  };

  // A step's stream is open only while its panel is: opened, it reads the
  // events from the first line again; closed, its lines leave the page.
  const follow = (p) => {
    if (p.d.open && !p.stop) {
      p.stop = listen(base + "/steps/" + p.st.n + "/events", (ev) => add(p.v, ev), () => {
        if (p.v) cancelAnimationFrame(p.v.frame);
        const v = view();
        p.v ? p.v.root.replaceWith(v.root) : p.body.append(v.root);
        p.v = v;
      });
    } else if (!p.d.open && p.stop) {
      p.stop();
      p.stop = null;
      cancelAnimationFrame(p.v?.frame);
      p.v?.root.remove();
      p.v = null;
    }
  };

  const panel = () => {
    const sum = el("summary");
    const err = el("p", "attention");
    const body = el("div", "more-body", err);
    const p = { d: el("details", "more", sum, body), sum, err, body, touched: false, stop: null, v: null };
    sum.addEventListener("click", () => { p.touched = true; });
    p.d.addEventListener("toggle", () => follow(p));
    return p;
  };

  const fill = (p, st, live) => {
    p.st = st;
    const facts = [st.agent, st.client, st.model].filter(Boolean);
    facts.push(new Date(st.start).toLocaleTimeString(), took(new Date(st.end || Date.now()) - new Date(st.start)));
    p.sum.replaceChildren(el("span", "name", st.kind), el("span", "fact", facts.join(" · ")), outcome(st.outcome, live && !st.end));
    p.err.textContent = st.error;
    p.err.hidden = !st.error;
  };

  // The steps' panels by number, kept across reads so an open one stays open.
  const panels = new Map();
  const render = (r) => {
    head(r);
    r.steps.forEach((st, i) => {
      let p = panels.get(st.n);
      if (!p) {
        panels.set(st.n, p = panel());
        $("steps").append(p.d);
      }
      fill(p, st, r.live);
      // The newest step is open and the others closed, until the user opens or closes one.
      if (!p.touched) p.d.open = i === r.steps.length - 1;
    });
    $("missing").hidden = true;
    $("steps-section").hidden = false;
  };

  let live = true;
  const load = () => fetch(base).then((r) => {
    if (r.status === 401) location.reload(); // the token was replaced: the daemon's answer says what to do
    if (r.status === 404) return null;
    if (!r.ok) throw new Error(r.statusText);
    return r.json();
  }).then((r) => {
    reachable(true);
    if (!r) {
      live = false;
      missing("This run is not on disk: it may have been deleted, or the link is wrong.");
      return;
    }
    live = r.live;
    render(r);
  }, () => reachable(false));

  if (!project || !issue || !run) {
    missing("This link does not name a run. Open one from Runs.");
    return;
  }
  const visible = () => document.visibilityState === "visible";
  load();
  setInterval(() => live && visible() && load(), 3000);
  document.addEventListener("visibilitychange", () => live && visible() && load());
  // The browser closes the streams itself when the page is left; a page kept
  // for the back button gets them again from its open panels.
  addEventListener("pagehide", () => panels.forEach((p) => { if (p.stop) { p.stop(); p.stop = null; } }));
  addEventListener("pageshow", (e) => e.persisted && panels.forEach(follow));
})();
