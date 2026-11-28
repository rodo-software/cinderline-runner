// The Overview: draws the daemon's status from /api/status/stream (the JSON of
// localapi.Status) into index.html.
(() => {
  const $ = (id) => document.getElementById(id);
  const plural = (n, one, many) => n + " " + (n === 1 ? one : many);

  // when is time t as "3 minutes ago", with the exact time on hover.
  const when = (t) => {
    const n = el("time", "", ago(t));
    n.dateTime = t;
    n.title = new Date(t).toLocaleString();
    return n;
  };

  const kv = (rows) => el("table", "kv", ...rows.map(([k, v]) => {
    const th = el("th", "", k);
    th.scope = "row";
    return el("tr", "", th, el("td", "", ...[v].flat()));
  }));

  // state is a project's status dot: the one fact that matters most.
  const state = (p, runs) => {
    if (!p.connected) return p.last_error ? el("span", "status error", "Not connected") : el("span", "status", "Connecting");
    if (p.draining) return el("span", "status warn", "Draining");
    if (p.waits_for_memory) return el("span", "status warn", "Waits for memory");
    return runs ? el("span", "status running", "Running") : el("span", "status ok", "Idle");
  };

  const runsText = (p) => {
    const team = (p.issues || []).length;
    if (!p.manager) return plural(team, "run", "runs");
    return team ? plural(team, "run", "runs") + " and the Project Manager" : "the Project Manager";
  };

  // A project's fold, kept across updates so it stays open and focused.
  const folds = new Map();

  const project = (key, p) => {
    let d = folds.get(key);
    if (!d) {
      d = el("details", "more", el("summary"), el("div", "more-body"));
      folds.set(key, d);
    }
    const runs = (p.issues || []).length + (p.manager ? 1 : 0);
    const poll = p.last_poll ? ["last poll ", when(p.last_poll)] : ["never polled"];
    d.firstChild.replaceChildren(
      el("span", "name", p.project),
      el("span", "fact", state(p, runs), " · " + runsText(p) + " · ", ...poll));
    const rows = [
      ["Connected", p.connected ? "Yes" : "No"],
      ["Last poll", p.last_poll ? when(p.last_poll) : "Never"],
      ["Runs in flight", (p.issues || []).join(", ") || "None"],
      ["Project Manager", p.manager || "Not running"],
    ];
    if (p.draining) rows.push(["Draining", "Takes the new config when its runs have finished"]);
    if (p.run_memory) rows.push(["Memory per run", bytes(p.run_memory) + (p.waits_for_memory ? ", waits for memory" : "")]);
    if (p.last_error) rows.push(["Last error", p.last_error], ["Last error at", when(p.last_error_at)]);
    d.lastChild.replaceChildren(kv(rows));
    return d;
  };

  const render = (st) => {
    const projects = st.projects || [];

    const needs = projects.filter((p) => p.last_error);
    $("attention").replaceChildren(...needs.length ? needs.map((p) => el("div", "attention",
      el("p", "what", p.project + ": " + p.last_error),
      el("p", "muted", (p.connected ? "Connected" : "Not connected") + " · last error ", when(p.last_error_at)))) : [el("p", "all-good", el("span", "status ok", "Nothing right now"))]);

    const runs = projects.flatMap((p) => [
      ...(p.issues || []).map((ref) => [ref, p.project]),
      ...p.manager ? [[p.manager + " · Project Manager", p.project]] : [],
    ]);
    $("runs").replaceChildren(...runs.map(([title, key]) => el("li", "",
      el("span", "badge running", "run"), el("span", "title", title), el("span", "meta", key))));
    $("runs").hidden = !runs.length;
    $("no-runs").hidden = !!runs.length;

    $("lead").replaceChildren(
      needs.length ? el("span", "status error", plural(needs.length, "project needs", "projects need") + " you") : el("span", "status ok", "All is well"),
      el("span", "muted", plural(runs.length, "run", "runs") + " in flight"));

    // Two entries of the config can share a key; the second gets its own fold.
    const keys = projects.map((p, i) => projects.findIndex((q) => q.project === p.project) === i ? p.project : p.project + "\n" + i);
    for (const [key, d] of folds) if (!keys.includes(key)) { d.remove(); folds.delete(key); }
    const list = $("project-list");
    projects.forEach((p, i) => {
      const d = project(keys[i], p);
      if (list.children[i] !== d) list.insertBefore(d, list.children[i] || null); // moving a node only when needed keeps its focus
    });
    list.hidden = !projects.length;
    $("no-projects").hidden = !!projects.length;

    const m = st.memory;
    $("budget").hidden = !m;
    if (m) {
      const meter = $("budget").querySelector(".meter");
      meter.hidden = !m.budget;
      $("budget-label").replaceChildren(...m.budget
        ? ["Memory budget: ", el("strong", "", bytes(m.reserved) + " of " + bytes(m.budget)), " reserved for runs"]
        : ["Docker's memory is not known yet, so no run starts."]);
      meter.setAttribute("aria-valuemax", m.budget);
      meter.setAttribute("aria-valuenow", m.reserved);
      meter.setAttribute("aria-valuetext", bytes(m.reserved) + " of " + bytes(m.budget));
      meter.firstChild.style.width = Math.min(100, 100 * m.reserved / (m.budget || 1)) + "%";
    }

    $("version").textContent = st.version;
  };

  let last;
  listen("/api/status/stream", (st) => { last = st; render(st); });
  setInterval(() => last && render(last), 30000); // "3 minutes ago" moves on between events
})();
