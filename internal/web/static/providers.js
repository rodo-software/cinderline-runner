// Providers: the runner's model providers of /api/config, each row with its
// in-place change, its key and Remove, and the Add form.
(() => {
  const $ = (id) => document.getElementById(id);
  const api = (p) => "/api/providers/" + encodeURIComponent(p.name);
  const button = (text, cls, label, onClick) => {
    const b = el("button", cls, text);
    b.type = "button";
    b.setAttribute("aria-label", label);
    b.addEventListener("click", () => onClick(b));
    return b;
  };
  const input = (name, value, label) => {
    const i = el("input");
    i.type = "text";
    i.name = name;
    i.inputMode = "numeric";
    i.size = 4;
    i.value = value;
    i.setAttribute("autocomplete", "off");
    i.setAttribute("aria-label", label);
    return i;
  };
  // A count as sent: empty is 0, which the field shows as its placeholder.
  const count = (v) => v.trim() === "" ? 0 : number(v);

  const row = (p, i) => {
    // The change's fields sit in their own cells, tied to its form by form=.
    const id = "change-" + i;
    const free = el("input");
    free.type = "checkbox";
    free.setAttribute("role", "switch");
    free.checked = !!p.free;
    free.setAttribute("aria-label", "Free: " + p.name);
    const max = input("max_agents", p.max_agents ? String(p.max_agents) : "", "Most agents at once of " + p.name);
    max.placeholder = "no limit";
    const look = input("lookahead", p.lookahead ? String(p.lookahead) : "", "Lookahead of " + p.name);
    look.placeholder = "0";
    [free, max, look].forEach((f) => f.setAttribute("form", id));
    const save = el("button", "", "Save");
    save.type = "submit";
    save.setAttribute("aria-label", "Save " + p.name);
    const change = el("form", "", save);
    change.id = id;
    change.method = "post";
    change.addEventListener("submit", (e) => {
      e.preventDefault();
      act(save, "PUT", api(p), { free: free.checked, max_agents: count(max.value), lookahead: count(look.value) });
    });

    const key = el("input");
    key.type = "password";
    key.setAttribute("autocomplete", "off");
    key.setAttribute("aria-label", "New API key of " + p.name);
    const set = el("button", "", "Set");
    set.type = "submit";
    set.setAttribute("aria-label", "Set the API key of " + p.name);
    const keyForm = el("form", "row", key, set);
    keyForm.method = "post"; // a submit before this script ran never puts the key in the URL
    keyForm.addEventListener("submit", (e) => {
      e.preventDefault();
      secret(key, (value) => act(set, "PUT", api(p) + "/key", { value }));
    });
    if (p.has_key) {
      keyForm.append(button("Delete", "danger", "Delete the API key of " + p.name, (b) =>
        confirmed("Delete the API key of the provider " + p.name + "?", "Delete").then((yes) => yes && act(b, "DELETE", api(p) + "/key"))));
    }

    const name = el("th", "", p.name);
    name.scope = "row";
    return el("tr", "", name, el("td", "", p.base_url),
      el("td", "", el("label", "switch", free)), el("td", "", max), el("td", "", look), el("td", "", change),
      el("td", "", el("span", "status" + (p.has_key ? " ok" : ""), p.has_key ? "set" : "not set"), keyForm),
      el("td", "", button("Remove", "danger", "Remove " + p.name, (b) => confirmed("Remove the provider " + p.name
        + "?" + (p.has_key ? " Its API key is deleted too." : ""), "Remove")
        .then((yes) => yes && act(b, "DELETE", api(p))))));
  };

  // Drawn again only on a change, so a field being typed and a form's error stay.
  let last = "";
  const render = (providers) => {
    const json = JSON.stringify(providers);
    if (json === last) return;
    last = json;
    $("providers").replaceChildren(...providers.map(row));
    $("list").hidden = !providers.length;
    $("none").hidden = !!providers.length;
  };

  // The config is read on opening and on coming back to the page, as reading
  // it reads the keychain; the status, cheap, every 5 s, for the top bar.
  const load = () => getJSON("/api/config").then((c) => {
    reachable(true);
    render(c.config.providers || []);
  }, () => reachable(false));

  // act sends as send does and, on success, loads the config again.
  const act = (b, method, url, body) => send(b, method, url, body).then((ok) => ok && load().then(() => true));

  $("add").addEventListener("submit", (e) => {
    e.preventDefault();
    const f = e.currentTarget.elements;
    secret(f.key, (key) => act(e.submitter, "POST", "/api/providers", {
      name: f.name.value, base_url: f.base_url.value, free: f.free.checked,
      max_agents: count(f.max_agents.value), lookahead: count(f.lookahead.value), key,
    })).then((ok) => ok && e.target.reset());
  });

  const visible = () => document.visibilityState === "visible";
  load();
  setInterval(() => visible() && getJSON("/api/status").then(() => reachable(true), () => reachable(false)), 5000);
  document.addEventListener("visibilitychange", () => visible() && load());
})();
