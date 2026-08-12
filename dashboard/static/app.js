// throttle dashboard refresh.
//
// This file patches numbers that the server already formatted. It does no arithmetic on
// money, does not decide whether a cost is known, and does not build a table. Every
// currency figure in /api/summary arrives as a finished string precisely so that the
// rule keeping an unknown cost from printing as $0 has exactly one implementation, in
// Go.
//
// When the page's *shape* changes -- a new period, a gauge that stops having a numeric
// reading, an activity store that fails -- patching would leave a half-true page, so we
// reload instead and let the templates render the truth.

(function () {
  "use strict";

  var body = document.body;
  if (!body.dataset.budget) return; // first-run page: nothing to poll for.

  // Five seconds, fixed. A budget moves at the speed of requests, not of frames, and a
  // dashboard that polls harder than that is spending the operator's SQLite reads to
  // shorten a wait nobody is having. Deliberately not a configuration setting: it is a
  // property of how fast the numbers change, not a preference. No WebSocket or SSE either
  // -- a poll of a local read model is simpler than a push channel and cannot go stale
  // silently when the connection drops. ?refresh= stays as a development affordance.
  var params = new URLSearchParams(location.search);
  var seconds = parseInt(params.get("refresh"), 10);
  if (isNaN(seconds)) seconds = 5;
  if (seconds <= 0) return; // ?refresh=0 turns polling off.
  var interval = Math.max(2, seconds) * 1000;

  // Text fields: JSON key -> data-field attribute. Anything absent from the page is
  // skipped, so a template can drop a figure without breaking the poll.
  var TEXT = {
    at_display: "at",
    allocation: "allocation",
    carry_in: "carry-in",
    total: "total",
    spent: "spent",
    reserved: "reserved",
    remaining_allocation: ["remaining", "remaining-2"],
    target_by_now: "target",
    allowed_by_now: "allowed",
    pace_balance: "pace",
    spendable_now: "spendable",
    period_start: "period-start",
    period_end: "period-end",
    elapsed: "elapsed",
    time_remaining: ["time-remaining", "time-remaining-2"],
    average_burn_to_date: "average-burn",
    sustainable_burn: "sustainable-burn",
    burn_pressure: "pressure",
    straight_line_projection: "projection",
    straight_line_projection_note: "projection-note",
    bank_amount: "bank-amount",
    bank_label: "bank-label"
  };

  var url = "/api/summary?budget=" + encodeURIComponent(body.dataset.budget) +
    "&period=" + encodeURIComponent(body.dataset.period || "");

  // Shape signature. A change here means the rendered page structure is no longer the
  // right one, and the fix is a reload rather than a cleverer patch.
  function shape(d) {
    return [
      d.empty ? "empty" : "budget",
      d.period_id,
      d.burn_pressure_state,
      d.activity_available,
      d.overspent,
      d.live_holds,
      d.expired_holds,
      d.unresolved,
      d.outcome_unknown,
      d.awaiting_external
    ].join("|");
  }

  var baseline = null; // shape at page render
  var spentAtLoad = null;
  var failures = 0;

  function field(name) {
    return document.querySelectorAll('[data-field="' + name + '"]');
  }

  function setText(name, value) {
    if (value === undefined || value === null) return;
    var nodes = field(name);
    for (var i = 0; i < nodes.length; i++) {
      if (nodes[i].textContent !== value) nodes[i].textContent = value;
    }
  }

  function setAttrs(name, attrs) {
    var nodes = field(name);
    for (var i = 0; i < nodes.length; i++) {
      for (var k in attrs) {
        if (attrs[k] !== undefined && attrs[k] !== null) {
          nodes[i].setAttribute(k, attrs[k]);
        }
      }
    }
  }

  function apply(d) {
    for (var key in TEXT) {
      var targets = TEXT[key];
      if (typeof targets === "string") targets = [targets];
      for (var i = 0; i < targets.length; i++) setText(targets[i], d[key]);
    }

    // The gauge. The needle and arc are geometry the server computed from the same
    // basis points it printed, so the dial and the reading cannot disagree.
    setAttrs("gauge-needle", { x2: d.needle_x, y2: d.needle_y });
    if (typeof d.gauge_arc === "string") setAttrs("gauge-arc", { d: d.gauge_arc });

    var fill = field("bank-fill")[0];
    if (fill && typeof d.bank_fill_pct === "number") {
      fill.style.width = d.bank_fill_pct + "%";
      // Direction is a fact about the sign, not a colour choice.
      var borrowed = /BORROW/i.test(d.bank_label || "");
      fill.classList.toggle("left", borrowed);
      fill.classList.toggle("right", !borrowed);
    }

    var elapsed = field("elapsed-pct")[0];
    if (elapsed && d.elapsed_pct) elapsed.style.width = d.elapsed_pct + "%";

    staleness(d);
  }

  // The activity table, the chart, and the breakdowns are rendered once, server-side.
  // Rather than re-implement their completeness rules here, say plainly that they are
  // from page load once settled spend has moved.
  function staleness(d) {
    if (spentAtLoad === null || d.spent === spentAtLoad) return;
    if (document.getElementById("stale-notice")) return;
    var panel = document.querySelector(".activity");
    if (!panel) return;
    var p = document.createElement("p");
    p.id = "stale-notice";
    p.className = "notice info stale";
    p.appendChild(document.createTextNode(
      "Settled spend has moved since this page was rendered. The figures above are " +
      "current; the request table, chart, and breakdowns below are from page load. "));
    var a = document.createElement("a");
    a.href = location.href;
    a.textContent = "Reload";
    p.appendChild(a);
    var heading = panel.querySelector("h2");
    if (heading && heading.nextSibling) panel.insertBefore(p, heading.nextSibling);
    else panel.appendChild(p);
  }

  function poll() {
    if (document.hidden) return; // a hidden tab needs no figures.
    fetch(url, { headers: { Accept: "application/json" }, cache: "no-store" })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (d) {
        failures = 0;
        var sig = shape(d);
        if (baseline === null) {
          baseline = sig;
          spentAtLoad = d.spent;
        }
        if (sig !== baseline) {
          location.reload();
          return;
        }
        apply(d);
      })
      .catch(function () {
        // The server is gone or the ledger is unreadable. Leave the last known figures
        // on screen rather than blanking them, and stop hammering.
        failures++;
        if (failures >= 5) clearInterval(timer);
      });
  }

  var timer = setInterval(poll, interval);
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) poll();
  });
  poll();
})();
