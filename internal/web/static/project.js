// A project: its panels from /api/config, its state from /api/status and its
// secrets from /api/projects/{id}/keys, for the project of ?key=, &url= and
// &workspace= (projectHref). Each panel shows what is set, else the default
// that applies; its forms change it with send.
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
  const button = (text, cls, label, onClick) => {
    const b = el("button", cls, text);
    b.type = "button";
    b.setAttribute("aria-label", label);
    b.addEventListener("click", () => onClick(b));
    return b;
  };
  // A list of the project's own items, each with a Remove button that calls
  // remove, then the runner-wide ones, changed on Settings.
  const items = (own, all, show, label, remove) => own.length + all.length
    ? el("ul", "items", ...own.map((x) => el("li", "", show(x), button("Remove", "danger", "Remove " + label(x), (b) => remove(b, x)))),
      ...all.map((x) => {
        const a = el("a", "", "Settings");
        a.href = "/settings.html";
        return el("li", "", show(x), el("span", "muted small", "all projects, on ", a));
      }))
    : none("None.");
  // The project's routes, which name it by its ID.
  const api = (p) => "/api/projects/" + encodeURIComponent(p.id);
  // A secret field is emptied once its request is answered, refused or not,
  // so its value stays nowhere on the page.
  const secret = (input, request) => request(input.value).finally(() => { input.value = ""; });
  // A word for a shell: quoted unless it is plainly safe as it is.
  const sh = (s) => /^[\w.:/@%+=-]+$/.test(s) ? s : "'" + s.replaceAll("'", "'\\''") + "'";

  // The parts that change with the status, every 5 s.
  const drawStatus = (p, s) => {
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
  };

  // A field shows the project's own value, set again only when that changed,
  // so a value being typed stays; was keeps the value it last showed.
  const fill = (input, v) => {
    if (input.dataset.was === v) return;
    input.value = input.dataset.was = v;
  };

  // The parts that change with the config, drawn again only after a change,
  // so a form's error and a focused button stay.
  const drawConfig = (cfg, p, keys, cmd) => {
    const c = cfg.config;
    const d = cfg.defaults;
    document.title = p.key + " · Projects · Cinderline runner";
    $("title").textContent = p.key;

    const repos = p.extra_repositories || [];
    $("repos").replaceChildren(repos.length
      ? table(["Repository", "Ref", "Token", ""], repos.map((r) => [r.url, r.ref || el("span", "muted", "default branch"),
        el("span", "status" + (r.has_token ? " ok" : ""), r.has_token ? "set" : "not set"),
        button("Remove", "danger", "Remove " + r.url, (b) => confirmed("Remove the repository " + r.url + " from "
          + p.key + "? Its git token, if it has one, is deleted too.", "Remove").then((yes) => yes && act(b, "DELETE", api(p) + "/repos", { repo: r.url })))]))
      : none("None."));

    $("hosts").replaceChildren(items(p.allow_hosts || [], c.allow_hosts || [], (h) => el("code", "", h), (h) => h,
      (b, h) => act(b, "DELETE", "/api/hosts", { project: p.id, host: h })));
    $("skills").replaceChildren(items(p.allow_skills || [], c.allow_skills || [],
      (k) => el("span", "", el("strong", "", k.name), " ", el("span", "muted small", k.url)), (k) => k.name,
      (b, k) => act(b, "DELETE", "/api/skills", { project: p.id, name: k.name })));

    $("limits").replaceChildren(kv([
      ["Memory per agent container", p.memory || fallback(d.memory, "default")],
      ["Parallel runs", p.max_parallel_runs ? String(p.max_parallel_runs) : wide(c.max_parallel_runs, d.max_parallel_runs)],
      ["Transcript days", p.transcript_days != null ? String(p.transcript_days) : wide(c.transcript_days, d.transcript_days)],
    ]));
    const [memory, parallel, days] = ["memory", "parallel", "days"].map((id) => $(id).elements.value);
    fill(memory, p.memory || "");
    fill(parallel, p.max_parallel_runs ? String(p.max_parallel_runs) : "");
    fill(days, p.transcript_days != null ? String(p.transcript_days) : "");
    memory.placeholder = d.memory;
    parallel.placeholder = c.max_parallel_runs ?? d.max_parallel_runs;
    days.placeholder = c.transcript_days ?? d.transcript_days;

    $("secrets").replaceChildren(keys ? table(["Secret", "State", "", ""], keys.map((k) => {
      const url = api(p) + "/keys/" + encodeURIComponent(k.kind);
      // An extra repository's token, git-token:URL, is set with its repository:
      // the keys routes refuse it. The others are the two tokens and the
      // clients' model keys.
      const own = !k.kind.includes(":");
      const client = own && k.kind !== "runner-token" && k.kind !== "git-token";
      const input = el("input");
      input.type = "password";
      input.setAttribute("autocomplete", "off");
      input.setAttribute("aria-label", "New value of " + k.kind);
      const set = el("button", "", "Set");
      set.type = "submit";
      set.setAttribute("aria-label", "Set " + k.kind);
      const form = el("form", "row", input, set);
      form.method = "post";
      form.addEventListener("submit", (e) => {
        e.preventDefault();
        secret(input, (value) => act(set, "PUT", url, { value }));
      });
      return [
        client ? el("div", "", k.kind, el("p", "hint", "Setting it wins over an earlier login.")) : k.kind,
        el("span", "status" + (k.set ? " ok" : ""), k.set ? "set" : "not set"),
        own ? form : "",
        own && k.set ? button("Delete", "danger", "Delete " + k.kind, (b) => confirmed("Delete the secret " + k.kind
          + " of " + p.key + "?", "Delete").then((yes) => yes && act(b, "DELETE", url))) : "",
      ];
    })) : none("Not known: the runner did not answer."));

    $("login").replaceChildren(copyable(cmd));
  };

  let cfg, st, keys, name, cur, lastConfig = "", lastStatus = "";
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
    const p = cur = projects[i];
    const s = statuses(projects, st)[i];
    // The CLI needs -url and -workspace only when another project has the key.
    const shared = projects.some((o, j) => j !== i && o.key === p.key);
    const cmd = [name, "login", "CLIENT", "--project", p.key, ...shared ? ["-url", p.url, "-workspace", p.workspace] : []].map(sh).join(" ");
    // Drawn again only on a change, so a focused element keeps its focus.
    const c = JSON.stringify([cfg, p, keys, cmd]);
    if (c !== lastConfig) {
      lastConfig = c;
      drawConfig(cfg, p, keys, cmd);
    }
    const s2 = JSON.stringify([p, s && { ...s, last_poll: null }]);
    if (s2 !== lastStatus) {
      lastStatus = s2;
      drawStatus(p, s);
    }
  };

  // The config and the keys are read on opening and on coming back to the
  // page, as reading them reads the keychain; the status, cheap, every 5 s.
  const all = () => Promise.all([getJSON("/api/config"), getJSON("/api/name")]).then(([c, n]) => {
    cfg = c;
    name = n.name;
    const p = (c.config.projects || []).find(wanted);
    return p ? getJSON(api(p) + "/keys").then((k) => { keys = k; }, () => { keys = null; }) : null;
  });
  const load = (full) => Promise.all([full || !cfg ? all() : null, getJSON("/api/status")])
    .then(([, s]) => { reachable(true); st = s; render(); }, () => reachable(false));

  // act sends as send does and, on success, loads the page's data again.
  const act = (b, method, url, body) => send(b, method, url, body).then((ok) => ok && load(true).then(() => true));

  // A form's submit: send with the button that submitted it.
  const onSubmit = (id, then) => $(id).addEventListener("submit", (e) => {
    e.preventDefault();
    then(e.submitter, e.currentTarget.elements);
  });
  onSubmit("add-host", (b, f) => act(b, "POST", "/api/hosts", { project: cur.id, host: f.host.value })
    .then((ok) => ok && b.form.reset()));
  onSubmit("add-repo", (b, f) => secret(f.token, (token) => act(b, "POST", api(cur) + "/repos",
    { url: f.url.value, ref: f.ref.value, token })).then((ok) => ok && b.form.reset()));
  onSubmit("add-skill", (b, f) => act(b, "POST", "/api/skills", { project: cur.id, name: f.name.value, url: f.url.value })
    .then((ok) => ok && b.form.reset()));

  // A setting of the project, which the settings routes name by key, URL
  // and workspace. Save sends value(the field); the clear button sends
  // clear, which drops the project's own value. After either the field
  // shows what is set.
  const setting = (id, url, value, clear) => {
    const form = $(id);
    const input = form.elements.value;
    const put = (b, v) => act(b, "PUT", url, { project: cur.key, url: cur.url, workspace: cur.workspace, value: v })
      .then((ok) => { if (ok) input.value = input.dataset.was; });
    onSubmit(id, (b) => put(b, value(input.value)));
    form.querySelector("[data-clear]").addEventListener("click", (e) => put(e.currentTarget, clear));
  };
  // A whole number goes as a number; anything else as typed, for the daemon
  // to refuse.
  const number = (v) => /^-?\d+$/.test(v.trim()) ? Number(v) : v;
  setting("memory", "/api/settings/memory", (v) => v, "");
  setting("parallel", "/api/settings/parallel", number, 0);
  setting("days", "/api/settings/transcript-days", number, null);

  const visible = () => document.visibilityState === "visible";
  load(true);
  setInterval(() => visible() && load(false), 5000);
  document.addEventListener("visibilitychange", () => visible() && load(true));
})();
