// One run: its header and steps from /api/runs/{project}/{issue}/{run}, read
// again every 3 s while the run is live, for each open step its events, and
// while live its containers' CPU and memory.
// Every text here comes from an agent or an issue: it goes in as text only.
(() => {
  const $ = (id) => document.getElementById(id);
  const q = new URLSearchParams(location.search);
  const [project, issue, run] = ["project", "issue", "run"].map((k) => q.get(k) || "");
  const base = "/api/runs/" + [project, issue, run].map(encodeURIComponent).join("/");
  const maxLines = 20000; // out lines of one step kept in the page, and entries of its timeline
  const turns = ["run", "resolve"]; // the steps that are an agent turn

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

  // The Raw lines switch's last choice, the start of each agent turn opened after it.
  let rawLines = false;
  try { rawLines = localStorage.getItem("raw-lines") === "1"; } catch { /* storage off: the timeline */ }

  // scroller is a box that scrolls n, followed while live, with its jump button.
  const scroller = (n, label) => {
    n.tabIndex = 0;
    n.setAttribute("aria-label", label);
    const jump = el("button", "primary jump", "Jump to newest");
    jump.type = "button";
    jump.hidden = true;
    return { root: el("div", "follow", n, jump), keep: follower(n, jump) };
  };

  // view is what an open step shows of its events stream, filled as they
  // come: its out lines in the plain box and, for an agent turn, in a timeline.
  const view = (turn) => {
    const text = el("pre", "box");
    const prompt = el("details", "fold", el("summary", "", "Prompt"), text);
    const hosts = el("tbody");
    const cols = ["Host", "Reason", "Method", "URL"].map((h) => {
      const th = el("th", "", h);
      th.scope = "col";
      return th;
    });
    // A region with the focus, so its columns can be scrolled to by keyboard.
    const scroll = el("div", "scroll", el("table", "rows", el("thead", "", el("tr", "", ...cols)), hosts));
    scroll.tabIndex = 0;
    scroll.setAttribute("role", "region");
    scroll.setAttribute("aria-label", "Hosts refused");
    const refused = el("div", "", el("h3", "", "Hosts refused"), scroll);
    const earlier = el("p", "muted small");
    const out = el("pre", "box");
    out.setAttribute("role", "region");
    const lines = scroller(out, "Output lines");
    const v = { text, prompt, hosts, refused, earlier, out, lines, pending: [], dropped: 0, frame: 0, has: false };
    const kids = [prompt, refused, earlier, lines.root];
    if (turn) {
      v.tl = el("ol", "timeline box");
      v.timeline = scroller(v.tl, "Timeline");
      v.calls = new Map(); // a tool call's id: its row and running mark, until its result comes
      v.raw = el("input");
      v.raw.type = "checkbox";
      v.raw.setAttribute("role", "switch");
      v.raw.checked = rawLines;
      v.raw.addEventListener("change", () => {
        rawLines = v.raw.checked;
        try { localStorage.setItem("raw-lines", rawLines ? "1" : "0"); } catch { /* kept for this page only */ }
        show(v);
        const box = rawLines ? v.out : v.tl;
        box.scrollTop = box.scrollHeight;
      });
      v.toggle = el("p", "raw-toggle", el("label", "switch", v.raw, " Raw lines"));
      kids.splice(2, 0, v.toggle);
      kids.push(v.timeline.root);
    }
    for (const n of kids) n.hidden = true;
    v.root = el("div", "", ...kids);
    return v;
  };

  // show shows the plain box or the timeline, once there are lines.
  const show = (v) => {
    const raw = !v.tl || v.raw.checked;
    v.lines.root.hidden = !v.has || !raw;
    if (v.tl) {
      v.timeline.root.hidden = !v.has || raw;
      v.toggle.hidden = !v.has;
    }
  };

  const tokens = new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 });
  // usage is a usage event as "312K in · 14.1K out · $1.42", of what it reports.
  const usage = (e) => [e.input_tokens != null && tokens.format(e.input_tokens) + " in",
    e.output_tokens != null && tokens.format(e.output_tokens) + " out",
    e.cost_usd != null && "$" + e.cost_usd.toFixed(2)].filter(Boolean).join(" · ");

  const time = (t) => {
    const n = el("time", "time", new Date(t).toLocaleTimeString());
    n.dateTime = t;
    return n;
  };

  const peekLines = 3; // of a tool result, shown while it is closed

  // entries adds to f the timeline entries of out line ev, from the events
  // parsed from it (Web 20); a line without them, or a kind the page does not
  // know, is the raw line, once.
  const entries = (p, f, ev) => {
    const v = p.v;
    let raw = false;
    for (const e of ev.events?.length ? ev.events : [{ kind: "raw" }]) {
      switch (e.kind) {
        case "text":
          if (e.text) f.append(el("li", "", el("div", "line", el("span", "who", "Agent"), time(ev.t)), el("p", "said", e.text)));
          break;
        case "thinking":
          if (e.text) f.append(el("li", "thinking", el("details", "fold", el("summary", "", "Thinking"), el("p", "said", e.text))));
          break;
        case "tool_call": {
          const input = e.input ?? "";
          const mark = el("span", "badge running pending", "running");
          const li = el("li", "tool", el("details", "fold call",
            el("summary", "", el("span", "who", e.name ?? ""), el("code", "", input.split("\n", 1)[0]), mark, time(ev.t)),
            el("pre", "", input)));
          f.append(li);
          if (e.id) v.calls.set(e.id, { li, mark });
          break;
        }
        case "tool_result": {
          const text = e.text ?? "";
          const lines = text.trimEnd().split("\n");
          const sum = el("summary", "", !text ? "Result, empty" : "Result, " + lines.length + (lines.length === 1 ? " line" : " lines"));
          if (e.error) sum.append(el("span", "badge error", "error"));
          const res = el("div", e.error ? "result error" : "result", el("details", "fold", sum, el("pre", "", text)));
          if (text) res.append(el("pre", "peek", lines.slice(0, peekLines).join("\n") + (lines.length > peekLines ? "\n…" : "")));
          const call = e.id && v.calls.get(e.id);
          if (call?.li.parentNode) { // not dropped from the top of the timeline
            call.li.append(res);
            call.mark.remove();
            v.calls.delete(e.id);
          } else {
            f.append(el("li", "tool", el("div", "line", el("span", "who", "Result"), time(ev.t)), res));
          }
          break;
        }
        case "usage": {
          const u = usage(e);
          p.use.textContent = u;
          p.use.hidden = !u;
          if (u) f.append(el("li", "", el("p", "muted small", u)));
          break;
        }
        default:
          if (!raw) f.append(el("li", "unknown", el("pre", "muted", ev.line ?? "")));
          raw = true;
      }
    }
  };

  // flush draws a step's out lines that came since the last frame: a finished
  // step sends all its lines at once, and the page is laid out once a frame.
  const flush = (p) => {
    const v = p.v;
    v.frame = 0;
    const lines = v.pending.slice(-maxLines);
    v.dropped += v.pending.length - lines.length;
    v.pending = [];
    const f = document.createDocumentFragment();
    for (const ev of lines) f.append(el("span", "", (ev.line ?? "") + "\n")); // a span a line, to drop the oldest
    v.lines.keep(() => {
      v.out.append(f);
      for (; v.out.childElementCount > maxLines; v.dropped++) v.out.firstElementChild.remove();
    });
    if (v.tl) {
      v.timeline.keep(() => { // in here, as a result goes under its call, which may be on the page
        const t = document.createDocumentFragment();
        for (const ev of lines) entries(p, t, ev);
        v.tl.append(t);
        while (v.tl.childElementCount > maxLines) v.tl.firstElementChild.remove();
      });
    }
    v.earlier.textContent = v.dropped + (v.dropped === 1 ? " earlier line" : " earlier lines") + " not shown";
    v.earlier.hidden = !v.dropped;
    v.has = true;
    show(v);
  };

  const add = (p, ev) => {
    const v = p.v;
    if (ev.type === "prompt") {
      v.text.textContent = ev.text ?? "";
      v.prompt.hidden = false;
    } else if (ev.type === "refused") {
      v.hosts.append(el("tr", "", ...[ev.host, ev.reason, ev.method, ev.url].map((s) => el("td", "", s ?? ""))));
      v.refused.hidden = false;
    } else if (ev.type === "out") {
      // A hidden tab draws no frames: drop the oldest half once twice the kept lines wait.
      if (v.pending.push(ev) >= 2 * maxLines) v.dropped += v.pending.splice(0, maxLines).length;
      v.frame ||= requestAnimationFrame(() => flush(p));
    }
  };

  // A step's stream is open only while its panel is: opened, it reads the
  // events from the first line again; closed, its lines leave the page.
  const follow = (p) => {
    if (p.d.open && !p.stop) {
      p.stop = listen(base + "/steps/" + p.st.n + "/events", (ev) => add(p, ev), () => {
        if (p.v) cancelAnimationFrame(p.v.frame);
        const v = view(turns.includes(p.st.kind));
        v.root.classList.toggle("live", p.running);
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
    const use = el("span", "fact"); // the latest usage its events reported
    use.hidden = true;
    const p = { d: el("details", "more", sum, body), sum, err, body, use, touched: false, stop: null, v: null, running: false };
    sum.addEventListener("click", () => { p.touched = true; });
    p.d.addEventListener("toggle", () => follow(p));
    return p;
  };

  const fill = (p, st, live) => {
    p.st = st;
    const facts = [st.agent, st.client, st.model].filter(Boolean);
    facts.push(new Date(st.start).toLocaleTimeString(), took(new Date(st.end || Date.now()) - new Date(st.start)));
    p.running = live && !st.end;
    p.sum.replaceChildren(el("span", "name", st.kind), el("span", "fact", facts.join(" · ")), p.use, outcome(st.outcome, p.running));
    p.v?.root.classList.toggle("live", p.running); // shows the running mark of calls without a result
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

  // The containers' measurements, by name: the last keep within the last
  // period ms, kept while the page is, so a container back after a step goes on.
  const keep = 200, period = 10 * 60 * 1000;
  const history = new Map();
  const pct = (v) => (v < 10 ? v.toFixed(1) : Math.round(v)) + "%";

  // spark is a small graph of points [time, value] up to top, the period up
  // to end wide: x by time, so the minutes the page was hidden keep their width.
  const spark = (pts, end, top, label) => {
    const svg = (tag, attrs) => {
      const n = document.createElementNS("http://www.w3.org/2000/svg", tag);
      for (const k in attrs) n.setAttribute(k, attrs[k]);
      return n;
    };
    const x = (t) => (120 * (t - end + period) / period).toFixed(1);
    const xy = pts.map(([t, v]) => x(t) + "," + (29 - 28 * Math.min(v / (top || 1), 1)).toFixed(1)).join(" ");
    const s = svg("svg", { class: "spark", viewBox: "0 0 120 30", preserveAspectRatio: "none", role: "img", "aria-label": label });
    s.append(svg("polygon", { class: "area", points: x(pts[0][0]) + ",30 " + xy + " 120,30" }), svg("polyline", { class: "line", points: xy }));
    return s;
  };

  const row = (c, end) => {
    const h = history.get(c.name);
    const highCPU = Math.max(...h.map((m) => m.cpu));
    const highMem = Math.max(...h.map((m) => m.mem));
    const limit = c.memory_limit_bytes; // 0: docker gave none, the graph's top is the highest seen
    const cpu = el("div", "", el("div", "muted small", "CPU"), el("div", "stat", pct(c.cpu_percent)),
      spark(h.map((m) => [m.t, m.cpu]), end, Math.max(100, highCPU),
        `${c.name} CPU, last 10 minutes: now ${pct(c.cpu_percent)}, highest ${pct(highCPU)}`));
    const used = el("div", "stat", bytes(c.memory_bytes));
    const mem = el("div", "", el("div", "muted small", "Memory"), used);
    if (limit) {
      const share = c.memory_bytes / limit;
      used.append(el("span", "muted small", " of " + bytes(limit)));
      if (share > 0.85) used.append(el("span", "near small", " near the limit"));
      const bar = el("div", share > 0.85 ? "meter warn" : "meter", el("span"));
      bar.setAttribute("role", "meter");
      bar.setAttribute("aria-label", c.name + " memory");
      bar.setAttribute("aria-valuemin", 0);
      bar.setAttribute("aria-valuemax", limit);
      bar.setAttribute("aria-valuenow", c.memory_bytes);
      bar.setAttribute("aria-valuetext", bytes(c.memory_bytes) + " of " + bytes(limit));
      bar.firstChild.style.width = Math.min(100, 100 * share) + "%";
      mem.append(bar);
    }
    mem.append(spark(h.map((m) => [m.t, m.mem]), end, limit || highMem,
      `${c.name} memory, last 10 minutes: now ${bytes(c.memory_bytes)}, highest ${bytes(highMem)}`));
    return el("li", "", el("div", "name", c.name), cpu, mem);
  };

  // stats shows one event of the stats stream (Web 26): its containers, or its error.
  const stats = (ev) => {
    $("stats-error").textContent = ev.error ?? "";
    $("stats-error").hidden = !ev.error;
    $("ctrs").hidden = !!ev.error;
    $("stats-none").hidden = !!ev.error || ev.containers.length > 0;
    $("stats-none").textContent = "No container runs now.";
    if (ev.error) return;
    const end = new Date(ev.time).getTime();
    for (const c of ev.containers) {
      const h = history.get(c.name) ?? [];
      h.push({ t: end, cpu: c.cpu_percent, mem: c.memory_bytes });
      while (h.length > keep || h[0].t < end - period) h.shift();
      history.set(c.name, h);
    }
    $("ctrs").replaceChildren(...ev.containers.map((c) => row(c, end)));
  };

  // The stats stream is open while the page shows a live run, so docker is
  // asked only then (decision 2026-11-26-local-web-page).
  let live = true, stopStats = null;
  const watch = () => {
    const on = live && visible();
    if (on && !stopStats) stopStats = listen("/api/runs/" + [project, issue].map(encodeURIComponent).join("/") + "/stats", stats);
    if (!on && stopStats) { stopStats(); stopStats = null; }
    $("containers").hidden = !live;
  };

  const load = () => fetch(base).then((r) => {
    if (r.status === 401) location.reload(); // the token was replaced: the daemon's answer says what to do
    if (r.status === 404) return null;
    if (!r.ok) throw new Error(r.statusText);
    return r.json();
  }).then((r) => {
    reachable(true);
    live = !!r?.live;
    watch();
    if (!r) {
      missing("This run is not on disk: it may have been deleted, or the link is wrong.");
      return;
    }
    render(r);
  }, () => reachable(false));

  if (!project || !issue || !run) {
    missing("This link does not name a run. Open one from Runs.");
    return;
  }
  const visible = () => document.visibilityState === "visible";
  load();
  setInterval(() => live && visible() && load(), 3000);
  document.addEventListener("visibilitychange", () => {
    watch();
    if (live && visible()) load();
  });
  // The browser closes the streams itself when the page is left; a page kept
  // for the back button gets them again from its open panels.
  addEventListener("pagehide", () => {
    panels.forEach((p) => { if (p.stop) { p.stop(); p.stop = null; } });
    if (stopStats) { stopStats(); stopStats = null; }
  });
  addEventListener("pageshow", (e) => {
    if (!e.persisted) return;
    panels.forEach(follow);
    watch();
  });
})();
