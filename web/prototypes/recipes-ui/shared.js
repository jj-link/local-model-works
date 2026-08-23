/* Shared behavior for the three Library → Recipes UI samples.
   Pure filtering/search/sort + state resolution + dialogs. Samples own
   rendering and layout; nothing here touches the network or /api/. */
(function (global) {
  "use strict";

  var STATES = ["populated", "loading", "empty", "error"];

  function parseState() {
    var p = new URLSearchParams(global.location.search);
    var s = p.get("state");
    return STATES.indexOf(s) !== -1 ? s : "populated";
  }

  function shortDigest(d) {
    if (!d) return "—";
    return d.length > 12 ? d.slice(0, 12) + "…" : d;
  }

  function wallClock(iso) {
    if (!iso) return "—";
    var d = new Date(iso);
    function p(n) { return String(n).padStart(2, "0"); }
    return d.toISOString().slice(0, 10) + " " + p(d.getUTCHours()) + ":" +
      p(d.getUTCMinutes()) + ":" + p(d.getUTCSeconds()) + "Z";
  }

  var SOURCE_ICON = { catalog: "◈", oci: "◉", git: "⎇", local: "▣" };
  var SOURCE_LABEL = { catalog: "Catalog", oci: "OCI", git: "Git", local: "Local" };
  var TRUST = {
    verified: { label: "verified", icon: "✓", rank: 0 },
    local: { label: "local", icon: "◉", rank: 1 },
    untrusted: { label: "untrusted", icon: "!", rank: 2 },
  };

  /* Pure filter/sort pipeline. All samples share this to guarantee parity. */
  function filterSort(list, opts) {
    opts = opts || {};
    var q = (opts.q || "").trim().toLowerCase();
    var trusts = opts.trusts || [];
    var sources = opts.sources || [];
    var node = opts.node || 0;

    var out = list.filter(function (r) {
      if (trusts.length && trusts.indexOf(r.trust_state) === -1) return false;
      if (sources.length && sources.indexOf(r.source.type) === -1) return false;
      if (node && r.compatibility.node_count !== node) return false;
      if (q) {
        var hay = (r.name + " " + r.source.type + " " + r.source.remote + " " + r.digest)
          .toLowerCase();
        if (hay.indexOf(q) === -1) return false;
      }
      return true;
    });

    var key = opts.sort || "name";
    out = out.slice().sort(function (a, b) {
      if (key === "installed") {
        return new Date(b.installed_at).getTime() - new Date(a.installed_at).getTime();
      }
      if (key === "trust") {
        return (TRUST[a.trust_state].rank - TRUST[b.trust_state].rank);
      }
      return a.name.toLowerCase().localeCompare(b.name.toLowerCase());
    });
    return out;
  }

  function countActiveFilters(opts) {
    var n = opts.q && opts.q.trim() ? 1 : 0;
    n += (opts.trusts || []).length;
    n += (opts.sources || []).length;
    if (opts.node) n += 1;
    return n;
  }

  /* Apply one of the four fixture states to a host container. */
  function applyState(host, state, count) {
    var banner = host.querySelector("[data-state-banner]");
    var list = host.querySelector("[data-list]");
    if (banner) {
      var icon = banner.querySelector("[data-state-icon]");
      var title = banner.querySelector("[data-state-title]");
      var body = banner.querySelector("[data-state-body]");
      if (state === "populated") {
        banner.hidden = true;
        banner.setAttribute("aria-hidden", "true");
        if (list) list.hidden = false;
      } else {
        banner.hidden = false;
        banner.setAttribute("aria-hidden", "false");
        banner.setAttribute("role", "status");
        if (list) list.hidden = true;
        var data = {
          loading: { icon: "…", title: "Loading recipes", body: "Contacting the recipe registry…" },
          empty: { icon: "∅", title: "No recipes match", body: "Clear filters or search to widen the result set." },
          error: { icon: "!", title: "Cannot load recipes", body: "The recipe service did not respond. Retry when the service is reachable." },
        }[state];
        if (icon) icon.textContent = data.icon;
        if (title) title.textContent = data.title;
        if (body) body.textContent = data.body;
      }
    }
    var counter = host.querySelector("[data-result-count]");
    if (counter) counter.textContent = state === "populated" ? String(count) : "—";
  }

  /* Mock Import Recipe dialog — purely presentational, no network request. */
  function openImport(host) {
    var d = host.querySelector("[data-import-dialog]");
    if (!d) return;
    d.hidden = false;
    d.setAttribute("aria-hidden", "false");
    var prior = host.querySelector("[data-import-source]");
    if (prior) prior.focus();
  }
  function closeImport(host) {
    var d = host.querySelector("[data-import-dialog]");
    if (!d) return;
    d.hidden = true;
    d.setAttribute("aria-hidden", "true");
    var trig = host.querySelector("[data-import-trigger]");
    if (trig) trig.focus();
  }

  /* Close the topmost overlay (import dialog first, then detail). */
  function bindEscape(host) {
    host.addEventListener("keydown", function (e) {
      if (e.key !== "Escape") return;
      var d = host.querySelector("[data-import-dialog]");
      if (d && !d.hidden) {
        closeImport(host);
        e.preventDefault();
        return;
      }
      var det = host.querySelector("[data-detail]");
      if (det && !det.hidden) {
        global.dispatchEvent(new CustomEvent("lmw:close-detail"));
        e.preventDefault();
      }
    });
  }

  global.LMW = {
    STATES: STATES,
    parseState: parseState,
    shortDigest: shortDigest,
    wallClock: wallClock,
    SOURCE_ICON: SOURCE_ICON,
    SOURCE_LABEL: SOURCE_LABEL,
    TRUST: TRUST,
    filterSort: filterSort,
    countActiveFilters: countActiveFilters,
    applyState: applyState,
    openImport: openImport,
    closeImport: closeImport,
    bindEscape: bindEscape,
  };
})(window);
