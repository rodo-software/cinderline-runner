// The Log: the runner's log from /api/log (one slog JSON line per event),
// newest at the bottom, followed unless the user scrolled up.
(() => {
  const max = 5000; // lines kept in the page, as the most /api/log sends
  const list = document.getElementById("lines");
  const jump = document.getElementById("jump");
  const filter = document.getElementById("filter");
  const count = document.getElementById("count");

  const levels = { ERROR: "error", WARN: "warn", INFO: "running" };
  const keep = follower(list, jump);

  // matches says whether a line's text holds the filter's, case aside.
  const matches = (li) => li.dataset.text.includes(filter.value.trim().toLowerCase());

  const showCount = () => {
    const all = list.children.length;
    const shown = filter.value.trim() ? [...list.children].filter((li) => !li.hidden).length : all;
    count.textContent = shown === all ? all + (all === 1 ? " line" : " lines") : shown + " of " + all + " lines";
  };

  const line = (rec) => {
    const { time, level, msg, ...attrs } = rec;
    const t = el("time", "time", time ? new Date(time).toLocaleTimeString() : "");
    if (time) { t.dateTime = time; t.title = new Date(time).toLocaleString(); }
    const lv = String(level ?? "");
    const kv = Object.entries(attrs).map(([k, v]) => k + "=" + (typeof v === "string" ? v : JSON.stringify(v)));
    const li = el("li", "", t,
      el("span", "badge " + (levels[lv.replace(/[+-]\d+$/, "")] || ""), lv),
      el("span", "msg", String(msg ?? "")),
      ...kv.map((s) => el("span", "attr", s)));
    li.dataset.text = [lv, msg, ...kv].join(" ").toLowerCase();
    return li;
  };

  const add = (rec) => {
    const li = line(rec);
    li.hidden = !matches(li);
    keep(() => {
      list.append(li);
      if (list.children.length > max) list.firstElementChild.remove();
    });
    showCount();
  };

  filter.form.addEventListener("submit", (e) => e.preventDefault()); // Enter must not reload the page
  filter.addEventListener("input", () => {
    for (const li of list.children) li.hidden = !matches(li);
    list.scrollTop = list.scrollHeight;
    showCount();
  });

  // Each connection starts with the tail again, so a reconnect starts afresh.
  listen("/api/log", add, () => { list.replaceChildren(); jump.hidden = true; showCount(); });
})();
