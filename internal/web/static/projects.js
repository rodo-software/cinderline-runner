// The Projects: each project of /api/config, with its state from /api/status,
// the status read again every 5 s while the page is visible.
(() => {
  const $ = (id) => document.getElementById(id);

  const row = (p, s) => {
    const link = el("a", "", p.key);
    link.href = projectHref(p);
    const key = el("th", "", link);
    key.scope = "row";
    const runs = s ? runCount(s) : 0;
    return el("tr", "", key, el("td", "", p.url), el("td", "", p.workspace), el("td", "", p.repository),
      el("td", "", s ? projectState(s, runs) : el("span", "status", "Not started")),
      el("td", "num", String(runs)));
  };

  // The rows are drawn again only when what they show changed, so a focused
  // link keeps its focus.
  let cfg, st, last = "";
  const render = () => {
    const projects = cfg.config.projects || [];
    const rows = statuses(projects, st).map((s, i) => [projects[i], s]);
    const json = JSON.stringify(rows.map(([p, s]) => [p.key, p.url, p.workspace, p.repository,
      s && [s.connected, !!s.last_error, s.draining, s.waits_for_memory, runCount(s)]]));
    if (json === last) return;
    last = json;
    $("projects").replaceChildren(...rows.map(([p, s]) => row(p, s)));
    $("list").hidden = !projects.length;
    $("none").hidden = !!projects.length;
  };

  // The config is read on opening and on coming back to the page, as reading
  // it reads the keychain; the status, cheap, every 5 s.
  const load = (all) => Promise.all([all || !cfg ? getJSON("/api/config") : cfg, getJSON("/api/status")])
    .then(([c, s]) => { reachable(true); cfg = c; st = s; render(); }, () => reachable(false));

  const visible = () => document.visibilityState === "visible";
  load(true);
  setInterval(() => visible() && load(false), 5000);
  document.addEventListener("visibilitychange", () => visible() && load(true));
})();
