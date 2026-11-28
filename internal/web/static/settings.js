// Settings: the runner-wide values of /api/config, each panel with the
// forms that change it with send. Every route here gets no project, or an
// empty one: the runner-wide value.
(() => {
  const $ = (id) => document.getElementById(id);
  const none = (text) => el("p", "muted", text);
  const kv = (rows) => el("table", "kv", ...rows.map(([k, v]) => {
    const th = el("th", "", k);
    th.scope = "row";
    return el("tr", "", th, el("td", "", ...[v].flat()));
  }));
  const fallback = (v, def) => v != null && v !== "" ? String(v) : [String(def), " ", el("span", "muted small", "(default)")];
  const now = (v, def) => v != null && v !== "" ? "Set to " + v + "." : "Not set: the default, " + def + ", applies.";
  // A list of items, each with a Remove button that calls remove.
  const items = (all, show, label, remove) => all.length
    ? el("ul", "items", ...all.map((x) => {
      const b = el("button", "danger", "Remove");
      b.type = "button";
      b.setAttribute("aria-label", "Remove " + label(x));
      b.addEventListener("click", () => remove(b, x));
      return el("li", "", show(x), b);
    }))
    : none("None.");
  // A field shows what is set, set again only when that changed, so a value
  // being typed stays; was keeps the value it last showed.
  const fill = (input, v) => {
    if (input.dataset.was === v) return;
    input.value = input.dataset.was = v;
  };
  // The page's port, from web_addr, 127.0.0.1:port.
  const portOf = (addr) => addr.slice(addr.lastIndexOf(":") + 1);

  let cfg, name, last = "";
  const render = () => {
    // Drawn again only on a change, so a focused button and a form's error stay.
    const json = JSON.stringify(cfg);
    if (json === last) return;
    last = json;
    const c = cfg.config;
    const d = cfg.defaults;
    $("panels").hidden = false;

    $("strict").elements.on.checked = !!c.strict_hosts;
    $("hosts").replaceChildren(items(c.allow_hosts || [], (h) => el("code", "", h), (h) => h,
      (b, h) => act(b, "DELETE", "/api/hosts", { project: "", host: h })));
    $("skills").replaceChildren(items(c.allow_skills || [],
      (k) => el("span", "", el("strong", "", k.name), " ", el("span", "muted small", k.url)), (k) => k.name,
      (b, k) => act(b, "DELETE", "/api/skills", { project: "", name: k.name })));

    // max_parallel_runs 0 is unset, as transcript_max_size "" is.
    $("parallel-now").textContent = now(c.max_parallel_runs || null, d.max_parallel_runs);
    $("days-now").textContent = now(c.transcript_days, d.transcript_days);
    $("size-now").textContent = now(c.transcript_max_size, d.transcript_max_size);
    const [parallel, days, size, port] = ["parallel", "days", "size", "port"].map((id) => $(id).elements.value);
    fill(parallel, c.max_parallel_runs ? String(c.max_parallel_runs) : "");
    fill(days, c.transcript_days != null ? String(c.transcript_days) : "");
    fill(size, c.transcript_max_size || "");
    fill(port, portOf(cfg.web_addr));
    parallel.placeholder = d.max_parallel_runs;
    days.placeholder = d.transcript_days;
    size.placeholder = d.transcript_max_size;

    const here = el("a", "", location.protocol + "//" + location.hostname + ":" + portOf(cfg.web_addr) + "/");
    here.href = here.textContent;
    $("web-now").replaceChildren("This page is at ", here, ".");

    $("fixed").replaceChildren(kv([
      ["Keep sessions for", fallback(c.keep_sessions_for, d.keep_sessions_for)],
      ["Minimum free disk", fallback(c.min_free_disk, d.min_free_disk)],
    ]));
  };

  // The config is read on opening and on coming back to the page, as reading
  // it reads the keychain; the status, cheap, every 5 s, for the top bar.
  const load = () => Promise.all([getJSON("/api/config"), name ? null : getJSON("/api/name")]).then(([c, n]) => {
    reachable(true);
    cfg = c;
    if (n) {
      name = n.name;
      terminal();
    }
    render();
  }, () => reachable(false));

  // act sends as send does and, on success, loads the config again.
  const act = (b, method, url, body) => send(b, method, url, body).then((ok) => ok && load().then(() => true));

  const onSubmit = (id, then) => $(id).addEventListener("submit", (e) => {
    e.preventDefault();
    then(e.submitter, e.currentTarget.elements);
  });

  $("strict").elements.on.addEventListener("change", (e) => {
    const s = e.currentTarget;
    act(s, "PUT", "/api/settings/strict-hosts", { on: s.checked }).then((ok) => { if (!ok) s.checked = !s.checked; });
  });
  onSubmit("add-host", (b, f) => act(b, "POST", "/api/hosts", { project: "", host: f.host.value })
    .then((ok) => ok && b.form.reset()));
  onSubmit("add-skill", (b, f) => act(b, "POST", "/api/skills", { project: "", name: f.name.value, url: f.url.value })
    .then((ok) => ok && b.form.reset()));

  // A runner-wide setting: Save sends value(the field); Use the default sends
  // clear. After either the field shows what is set.
  const setting = (id, url, value, clear) => {
    const input = $(id).elements.value;
    const put = (b, v) => act(b, "PUT", url, { value: v }).then((ok) => { if (ok) input.value = input.dataset.was; });
    onSubmit(id, (b) => put(b, value(input.value)));
    $(id).querySelector("[data-clear]").addEventListener("click", (e) => put(e.currentTarget, clear));
  };
  setting("parallel", "/api/settings/parallel", number, 0);
  setting("days", "/api/settings/transcript-days", number, null);
  setting("size", "/api/settings/transcript-max-size", (v) => v, "");

  // A new port: the daemon answers, then this address stops answering, so
  // the page shows the new one instead of loading again. The cookie is the
  // host's, whatever the port, so the new address lets this browser in.
  onSubmit("port", (b, f) => send(b, "PUT", "/api/settings/web", { port: number(f.value.value) })
    .then((r) => r && r.json()).then((w) => {
      if (!w) return;
      const p = portOf(w.web_addr);
      if (p === portOf(cfg.web_addr)) return load();
      const a = el("a", "", location.protocol + "//" + location.hostname + ":" + p + location.pathname);
      a.href = a.textContent;
      $("moved").replaceChildren("The web page moved to ", a, ". This address answers no more.");
      $("moved").hidden = false;
      a.focus();
    }));

  // Off is a plain confirmation: the page cannot turn itself on again.
  $("off").elements.off.addEventListener("change", async (e) => {
    const s = e.currentTarget;
    if (!s.checked) return;
    const cmd = name + " web on";
    const yes = await confirmed("Turn the web page off? This page stops answering. To turn it on again, type "
      + cmd + " in a terminal.", "Turn off");
    if (!yes || !await send(s, "PUT", "/api/settings/web", { on: false })) {
      s.checked = false;
      return;
    }
    s.disabled = true;
    $("is-off").replaceChildren(el("p", "", "The web page is off. To turn it on again, type this in a terminal:"), copyable(cmd));
    $("is-off").hidden = false;
  });

  // The Runner panel: the version and the state of the daemon's own actions,
  // from the status stream. from is the version this browser updated from,
  // kept over the reload after the runner restarts with the new one.
  let from = null, seen, draining = false, said = "";
  try { from = sessionStorage.getItem("update-from"); } catch { /* storage off: kept for this page only */ }
  seen = !!from; // a page loaded after the update began has its outcome in its first status
  const keep = (v) => {
    from = v;
    try { if (v) sessionStorage.setItem("update-from", v); else sessionStorage.removeItem("update-from"); } catch { /* as above */ }
  };
  let st;
  const runnerNow = () => {
    if (!st) return;
    const f = $("runner").elements;
    const u = st.update || "";
    $("version").textContent = "Version " + st.version;
    f.update.disabled = u === "running";
    if (u === "running") seen = true;
    else if (from && st.version !== from) {
      said = "Updated to " + st.version + ".";
      keep(null);
    } else if (from && seen) {
      said = "Already at the latest release.";
      keep(null);
    }
    const runs = (st.projects || []).flatMap((p) => [...p.issues || [], ...p.manager ? [p.manager] : []]);
    const [text, cls] = u === "running" ? ["Updating… The runner restarts with the new version once its runs in flight have finished.", ""]
      : u.startsWith("failed:") ? ["The update failed:" + u.slice("failed:".length), "field-error"]
        : draining ? ["Draining: the runner stops once its runs in flight have finished" + (runs.length ? ": " + runs.join(", ") + "." : "."), ""]
          : [said, ""];
    $("runner-now").textContent = text;
    $("runner-now").className = cls;
  };

  // Each action of the Runner panel: a plain confirmation, then the POST.
  const action = (button, question, yes, url, then) => $("runner").elements[button].addEventListener("click", async (e) => {
    const b = e.currentTarget;
    if (await confirmed(question, yes) && await send(b, "POST", url)) then();
  });
  action("update", "Update the runner to the latest release? Runs in flight finish first; then the runner restarts with the new version.",
    "Update", "/api/daemon/update", () => {
      seen = false; // a status sent before the update began says nothing of it
      said = "";
      keep(st.version);
    });
  action("drain", "Stop the runner once its runs in flight have finished? Until then it takes no new work.",
    "Stop after the runs", "/api/daemon/drain", () => { draining = true; runnerNow(); });
  action("stop", "Stop the runner now? Runs in flight are cut off.",
    "Stop now", "/api/daemon/stop", () => { said = "Stopping…"; runnerNow(); });
  action("rotate-ca", "Replace the runner's own CA? The egress proxy signs certificates with it for the hosts it refuses, "
    + "so Cinderline can show the URL an agent tried. Agent containers started from now on trust the new one; those running keep the old one.",
  "Replace the CA", "/api/daemon/rotate-ca", () => {
    said = "Replaced the CA. Agent containers started from now on trust the new one; those running keep the old one.";
    runnerNow();
  });

  // In the terminal: what the page does not offer, each command with why.
  const terminal = () => $("terminal").replaceChildren(...[
    [["service install", "service uninstall"], "Starts the runner at login, or no longer: the service runs the runner, so the runner does not install it."],
    [["uninstall"], "Removes everything: the service, containers, volumes, images, config and secrets."],
    [["login CLIENT --project KEY"], "Signs a client CLI in with its own login, which needs a terminal to answer it."],
    [["run"], "Starts the runner: this page is the runner's, so it cannot."],
  ].flatMap(([cmds, why]) => [...cmds.map((c) => copyable(name + " " + c)), el("p", "hint", why)]));

  const visible = () => document.visibilityState === "visible";
  load();
  listen("/api/status/stream", (s) => { st = s; runnerNow(); });
  document.addEventListener("visibilitychange", () => visible() && load());
})();
