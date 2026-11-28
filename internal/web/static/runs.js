// The Runs: the list of /api/runs (newest first), the runs in flight above the
// finished ones, read again every 5 s while the page is visible.
(() => {
  const $ = (id) => document.getElementById(id);
  const select = $("project");

  const build = (r) => {
    const link = el("a", "", r.issue);
    link.href = runHref(r);
    const issue = el("td", "", link);
    if (r.issue_url) {
      const c = el("a", "small", "Cinderline ↗");
      c.href = r.issue_url;
      c.target = "_blank";
      c.rel = "noopener";
      c.setAttribute("aria-label", "Open " + r.issue + " in Cinderline, in a new tab");
      issue.append(" ", c);
    }
    if (r.manager) issue.append(" ", el("span", "badge", "Project Manager"));
    const start = el("td");
    const dur = el("td", "num");
    const tr = el("tr", "", issue, el("td", "", r.project), start, dur, el("td", "num", String(r.steps)),
      el("td", "end", outcome(r.outcome, r.live)));
    // "3 minutes ago" and a live run's duration move on with each refresh.
    const tick = () => {
      start.replaceChildren(when(r.start));
      dur.textContent = took(new Date(r.end || Date.now()) - new Date(r.start));
    };
    return { tr, tick };
  };

  // A run's row, kept while the run does not change, so a focused link keeps
  // its focus across refreshes.
  const rows = new Map();
  const key = (r) => r.project + "/" + r.issue + "/" + r.run;
  const row = (r) => {
    const json = JSON.stringify(r);
    let x = rows.get(key(r));
    if (!x || x.json !== json) rows.set(key(r), x = { json, ...build(r) });
    x.tick();
    return x.tr;
  };

  // place puts trs in body in order, moving a row only when needed.
  const place = (body, trs) => {
    trs.forEach((tr, i) => { if (body.children[i] !== tr) body.insertBefore(tr, body.children[i] || null); });
    while (body.children.length > trs.length) body.lastElementChild.remove();
  };

  let runs = [];
  let chosen = new URLSearchParams(location.search).get("project") || "";
  let options = "";

  const render = () => {
    const keys = [...new Set([...runs.map((r) => r.project), chosen].filter(Boolean))].sort();
    if (keys.join("\n") !== options) {
      options = keys.join("\n");
      select.replaceChildren(select.options[0], ...keys.map((k) => el("option", "", k)));
    }
    select.value = chosen;

    const shown = runs.filter((r) => !chosen || r.project === chosen);
    const live = shown.filter((r) => r.live);
    const done = shown.filter((r) => !r.live);
    place($("live"), live.map(row));
    place($("done"), done.map(row));
    const keep = new Set(runs.map(key));
    for (const k of rows.keys()) if (!keep.has(k)) rows.delete(k);
    $("live-runs").hidden = !live.length;
    $("done-runs").hidden = !done.length;
    $("no-runs").hidden = !!shown.length;
  };

  select.addEventListener("change", () => {
    chosen = select.value;
    const q = new URLSearchParams(location.search);
    if (chosen) q.set("project", chosen);
    else q.delete("project");
    history.replaceState(null, "", q.size ? "?" + q : location.pathname);
    render();
  });

  const load = () => fetch("/api/runs?limit=500").then((r) => {
    if (r.status === 401) location.reload(); // the token was replaced: the daemon's answer says what to do
    if (!r.ok) throw new Error(r.statusText);
    return r.json();
  }).then((list) => { reachable(true); runs = list; render(); }, () => reachable(false));

  const visible = () => document.visibilityState === "visible";
  load();
  setInterval(() => visible() && load(), 5000);
  document.addEventListener("visibilitychange", () => visible() && load());
})();
