// The one script of the local web page. Loaded in <head>, so the stored theme
// is set before the page is drawn.
(() => {
  // The light and dark switch (decision 2026-11-27-web-look). The page follows
  // the system until the switch is used; the choice is kept in this browser only.
  const root = document.documentElement;
  const system = matchMedia("(prefers-color-scheme: dark)");
  let stored = null;
  try { stored = localStorage.getItem("theme"); } catch { /* storage off: follow the system */ }
  if (stored === "light" || stored === "dark") root.dataset.theme = stored;

  const isDark = () => root.dataset.theme ? root.dataset.theme === "dark" : system.matches;

  // A link to a place inside a folded row opens that row. Chrome does this by
  // itself; Safari does not.
  const reveal = () => {
    const target = location.hash && document.getElementById(location.hash.slice(1));
    for (let d = target && target.closest("details"); d; d = d.parentElement.closest("details")) d.open = true;
    if (target) target.scrollIntoView();
  };

  document.addEventListener("DOMContentLoaded", () => {
    const switches = document.querySelectorAll("[data-theme-switch]");
    const show = () => switches.forEach((s) => { s.checked = isDark(); });
    show();
    system.addEventListener("change", show);
    switches.forEach((s) => s.addEventListener("change", () => {
      root.dataset.theme = s.checked ? "dark" : "light";
      try { localStorage.setItem("theme", root.dataset.theme); } catch { /* kept for this page only */ }
      show();
    }));

    reveal();
    addEventListener("hashchange", reveal);
  });
})();
