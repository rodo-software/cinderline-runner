// Add a project: the form goes to POST /api/projects as one request, then the
// page goes to the new project's page. A refusal shows the daemon's text at
// the form, which keeps its values but the tokens.
(() => {
  const form = document.getElementById("add");
  let ids = [];
  getJSON("/api/config").then((c) => {
    reachable(true);
    ids = (c.config.projects || []).map((p) => p.id);
  }, () => reachable(false));

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const f = form.elements;
    send(e.submitter, "POST", "/api/projects", {
      url: f.url.value, runner_token: f.runner_token.value, repository: f.repository.value, git_token: f.git_token.value,
    }).then((ok) => ok && added(ids).then((p) => location.assign(p ? projectHref(p) : "/projects.html"),
      () => location.assign("/projects.html")))
      // The secret-field rule of the project page: a token stays nowhere once answered.
      .finally(() => { f.runner_token.value = f.git_token.value = ""; });
  });
})();
