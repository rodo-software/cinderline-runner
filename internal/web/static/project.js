// A project: its panels from /api/config, its state from /api/status and its
// secrets from /api/projects/{id}/keys, for the project of ?key=, &url= and
// &workspace= (projectHref). Read-only: each panel shows what is set, else
// the default that applies.
(() => {
  const $ = (id) => document.getElementById(id);
  const q = new URLSearchParams(location.search);
  const want = { key: q.get("key"), url: q.get("url"), workspace: q.get("workspace") };
  const wanted = (p) => p.key === want.key && (want.url == null || p.url === want.url)
    && (want.workspace == null || p.workspace === want.workspace);

  const kv = (rows) => el("table", "kv", ...rows.map(([k, v]) => {
    const th = el("th", "", k);
    th.scope = "row";
    return el("tr", "", th, el("td", "", ...[v].flat()));
  }));
  const none = (text) => el("p", "muted", text);
  const fallback = (v, why) => [String(v), " ", el("span", "muted small", "(" + why + ")")];
  // A runner-wide value: set there, else the default.
  const wide = (v, def) => v != null ? fallback(v, "runner-wide") : fallback(def, "default");
  const table = (head, rows) => el("div", "scroll", el("table", "rows",
    el("thead", "", el("tr", "", ...head.map((h) => { const th = el("th", "", h); th.scope = "col"; return th; }))),
    el("tbody", "", ...rows.map((r) => el("tr", "", ...r.map((c) => el("td", "", c)))))));
  // A list of the project's own items, then the runner-wide ones.
  const items = (own, all, show) => own.length + all.length
    ? el("ul", "items", ...own.map((x) => el("li", "", show(x))),
      ...all.map((x) => el("li", "", show(x), el("span", "muted small", "all projects"))))
    : none("None.");
  // A word for a shell: quoted unless it is plainly safe as it is.
  const sh = (s) => /^[\w.:/@%+=-]+$/.test(s) ? s : "'" + s.replaceAll("'", "'\\''") + "'";

  const draw = (cfg, p, s, keys, cmd) => {
    const c = cfg.config;
    const d = cfg.defaults;
    document.title = p.key + " · Projects · Cinderline runner";
    $("title").textContent = p.key;
    const runs = s ? runCount(s) : 0;
    $("lead").replaceChildren(s ? projectState(s, runs) : el("span", "status", "Not started"),
      el("span", "", runs + (runs === 1 ? " run" : " runs") + " in flight"));
    $("runs-link").href = "/runs.html?" + new URLSearchParams({ project: p.key });

    $("project").replaceChildren(kv([
      ["Key", p.key],
      ["Cinderline", p.url],
      ["Workspace", p.workspace],
      ["Repository", p.repository],
      ["Connected", !s ? "Not started" : s.connected ? "Yes" : "No"],
      ["Runs in flight", s ? [...(s.issues || []), ...s.manager ? [s.manager + " (Project Manager)"] : []].join(", ") || "None" : "None"],
      ["Last error", s?.last_error ? [s.last_error, " ", when(s.last_error_at)] : "None"],
    ]));

    const repos = p.extra_repositories || [];
    $("repos").replaceChildren(repos.length
      ? table(["Repository", "Ref", "Token"], repos.map((r) => [r.url, r.ref || el("span", "muted", "default branch"),
        el("span", "status" + (r.has_token ? " ok" : ""), r.has_token ? "set" : "not set")]))
      : none("None."));

    $("hosts").replaceChildren(items(p.allow_hosts || [], c.allow_hosts || [], (h) => el("code", "", h)));
    $("skills").replaceChildren(items(p.allow_skills || [], c.allow_skills || [],
      (k) => el("span", "", el("strong", "", k.name), " ", el("span", "muted small", k.url))));

    $("limits").replaceChildren(kv([
      ["Memory per agent container", p.memory || fallback(d.memory, "default")],
      ["Parallel runs", p.max_parallel_runs ? String(p.max_parallel_runs) : wide(c.max_parallel_runs, d.max_parallel_runs)],
      ["Transcript days", p.transcript_days != null ? String(p.transcript_days) : wide(c.transcript_days, d.transcript_days)],
    ]));

    $("secrets").replaceChildren(keys ? table(["Secret", "State"], keys.map((k) => [k.kind,
      el("span", "status" + (k.set ? " ok" : ""), k.set ? "set" : "not set")])) : none("Not known: the runner did not answer."));

    $("login").replaceChildren(copyable(cmd));
  };

  let cfg, st, keys, name, last = "";
  const render = () => {
    const projects = cfg.config.projects || [];
    const i = projects.findIndex(wanted);
    $("panels").hidden = i < 0;
    $("missing").hidden = i >= 0;
    if (i < 0) {
      $("missing").textContent = "This runner has no project " + (want.key || "") + (want.url ? " at " + want.url : "")
        + (want.workspace ? " in workspace " + want.workspace : "") + ".";
      return;
    }
    const p = projects[i];
    const s = statuses(projects, st)[i];
    // The CLI needs -url and -workspace only when another project has the key.
    const shared = projects.some((o, j) => j !== i && o.key === p.key);
    const cmd = [name, "login", "CLIENT", "--project", p.key, ...shared ? ["-url", p.url, "-workspace", p.workspace] : []].map(sh).join(" ");
    // Drawn again only on a change, so a focused element keeps its focus.
    const json = JSON.stringify([cfg, p, s && { ...s, last_poll: null }, keys, cmd]);
    if (json === last) return;
    last = json;
    draw(cfg, p, s, keys, cmd);
  };

  // The config and the keys are read on opening and on coming back to the
  // page, as reading them reads the keychain; the status, cheap, every 5 s.
  const all = () => Promise.all([getJSON("/api/config"), getJSON("/api/name")]).then(([c, n]) => {
    cfg = c;
    name = n.name;
    const p = (c.config.projects || []).find(wanted);
    return p ? getJSON("/api/projects/" + encodeURIComponent(p.id) + "/keys").then((k) => { keys = k; }, () => { keys = null; }) : null;
  });
  const load = (full) => Promise.all([full || !cfg ? all() : null, getJSON("/api/status")])
    .then(([, s]) => { reachable(true); st = s; render(); }, () => reachable(false));

  const visible = () => document.visibilityState === "visible";
  load(true);
  setInterval(() => visible() && load(false), 5000);
  document.addEventListener("visibilitychange", () => visible() && load(true));
})();
