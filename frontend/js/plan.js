// ═══ The plan ════════════════════════════════════════════════════════════
// Three panes hold it: the Plan tab (assessment, the roadmap spine, the one
// task list, the history), the Calendar tab (calendar.js), and the Kit tab
// (what to get, and what the learner is actually using).
//
// Two things here are deliberate rather than incidental:
//   · A todo is a SERIES. It shows completedCount / plannedCount and a button
//     that logs exactly one session. Nothing here can cancel the whole thing.
//   · `insufficient` is a warning about room, never a verdict of impossible.
//     The backend never tells a learner their goal can't be done, so neither
//     does this.

import { $, el, clear, icon, ICONS, paragraphs, prefersReducedMotion } from "./dom.js";
import { t, tg, formatDate, dayCodeLabel, duration, locale } from "./i18n.js";
import { formatInstant, todayInZone, daysBetween } from "./dates.js";
import { state, eventsForTodo, nextOpenEvent, planTimezone, doneCount } from "./state.js";

let handlers = {};
let animateRows = true;

export function initPlan(h) {
  handlers = h || {};
}
// `false` for in-place refreshes, so logging one session does not replay
// every entrance animation on the page.
export function setAnimateRows(on) {
  animateRows = !!on;
}
function delay(node, i, step = 0.04, cap = 0.4) {
  if (animateRows && !prefersReducedMotion()) node.style.animationDelay = Math.min(i * step, cap) + "s";
}
// "Today" is decided in the plan's timezone, which the backend owns.
function today() {
  return todayInZone(planTimezone());
}

export function planFacts(p) {
  const tail = String(p.path || "").split(" · ");
  return {
    track: p.track || (p.path ? tail[0] : "") || "",
    level: p.level || "",
    weeks: p.weeksTotal || 0,
    days: Array.isArray(p.days) ? p.days : null,
    weeklyMinutes: Number.isFinite(p.weeklyMinutes) && p.weeklyMinutes > 0 ? p.weeklyMinutes : 0,
    hoursPerWeek: Number.isFinite(p.hoursPerWeek) && p.hoursPerWeek > 0 ? p.hoursPerWeek : 0,
    perDay: Array.isArray(p.perDay) ? p.perDay.filter((d) => d && Number.isFinite(d.minutes)) : [],
  };
}

// ═══ Plan tab ═══════════════════════════════════════════════════════════
export function renderPlan(p) {
  $("planEmpty").hidden = true;
  const box = $("planContent");
  box.hidden = false;
  clear(box);

  const intro = el("div", "plan-intro");
  if (p.assessment) intro.appendChild(paragraphs(p.assessment, "assessment"));
  intro.appendChild(feasibility(p));
  const alerts = alertBlock(p);
  if (alerts) intro.appendChild(alerts);
  box.appendChild(intro);

  // Roadmap and tasks sit side by side wherever there is room for both.
  const grid = el("div", "plan-grid");
  const colRoad = el("section");
  const colTasks = el("section");
  grid.appendChild(colRoad);
  grid.appendChild(colTasks);
  box.appendChild(grid);

  colRoad.appendChild(sectionLabel(t("roadmap")));
  colRoad.appendChild(spine(p));

  colTasks.appendChild(sectionLabel(t("tasks")));
  const todos = Array.isArray(p.todos) ? p.todos : [];
  const iso = today();
  todos.forEach((td, i) => colTasks.appendChild(todoRow(td, i, iso)));
  if (!todos.length) colTasks.appendChild(el("p", "muted", t("no_phases")));

  box.appendChild(history(p));
}

function sectionLabel(text) {
  return el("div", "section-label", text);
}

// ── Feasibility ──
const FEAS = {
  feasible: { cls: "ok", key: "feas_feasible" },
  tight: { cls: "tight", key: "feas_tight" },
  insufficient: { cls: "warn", key: "feas_insufficient" },
  unknown: { cls: "neutral", key: "feas_unknown" },
};
function feasibility(p) {
  const st = FEAS[String(p.feasibilityStatus || "unknown")] || FEAS.unknown;
  const box = el("div", `reality ${st.cls}`);
  const head = el("div", "reality-head");
  head.appendChild(el("span", "reality-k", t("feasibility")));
  head.appendChild(el("span", "reality-status", t(st.key)));
  box.appendChild(head);
  if (p.feasibility) box.appendChild(paragraphs(p.feasibility, "reality-body"));
  return box;
}

// ── What the learner has to be told ──
function alertBlock(p) {
  const items = [];
  if (p.missesDeadline) {
    const a = el("div", "alert warn");
    a.appendChild(icon(ICONS.warn, 16, { weight: 2.2 }));
    const body = el("div");
    const head = el("strong", "", t("misses_deadline"));
    if (Number.isFinite(p.deadlineSlipDays) && p.deadlineSlipDays > 0) {
      head.appendChild(el("span", "alert-sub", t("slip_days", { n: p.deadlineSlipDays })));
    }
    body.appendChild(head);
    // The backend's own words on the matter, shown as they arrived.
    if (p.deadlineNote) body.appendChild(el("p", "", p.deadlineNote));
    a.appendChild(body);
    items.push(a);
  }
  if (Number.isFinite(p.droppedSessions) && p.droppedSessions > 0) {
    const a = el("div", "alert");
    a.appendChild(icon(ICONS.warn, 16, { weight: 2.2 }));
    a.appendChild(el("div", "", t("sched_dropped", { n: p.droppedSessions })));
    items.push(a);
  }
  return items.length ? el("div", { class: "alerts", children: items }) : null;
}

// ── The signature: the roadmap as a spine ──
function spine(p) {
  const list = el("ol", "spine");
  const phases = Array.isArray(p.phases) ? p.phases : [];
  const byPhase = {};
  (Array.isArray(p.milestones) ? p.milestones : []).forEach((m) => {
    (byPhase[m.phase] = byPhase[m.phase] || []).push(m);
  });
  const usedKeys = new Set(phases.map((ph) => ph.key));

  phases.forEach((ph, i) => {
    const li = el("li", "node phase");
    delay(li, i, 0.06, 0.45);
    li.appendChild(el("span", "node-dot"));
    if (Number.isFinite(ph.weekStart) && Number.isFinite(ph.weekEnd) && ph.weekEnd > 0) {
      li.appendChild(el("span", "wk", `W${ph.weekStart}–${ph.weekEnd}`));
    }
    li.appendChild(el("h4", "", ph.title || ""));
    if (ph.summary) li.appendChild(el("div", "node-sum", ph.summary));
    const ms = byPhase[ph.key];
    if (ms && ms.length) li.appendChild(milestoneList(ms));
    list.appendChild(li);
  });

  // A milestone whose phase key is missing from `phases` still has to show.
  const orphans = (Array.isArray(p.milestones) ? p.milestones : []).filter((m) => !usedKeys.has(m.phase));
  if (orphans.length) {
    const li = el("li", "node phase");
    li.appendChild(el("span", "node-dot"));
    li.appendChild(el("span", "wk", t("plan_section_milestones")));
    li.appendChild(milestoneList(orphans));
    list.appendChild(li);
  }

  // The finish, as the last node on the spine.
  const fin = el("li", "node finish");
  const dot = el("span", "node-dot");
  dot.appendChild(icon(ICONS.flag, 13, { weight: 2.4 }));
  fin.appendChild(dot);
  fin.appendChild(el("span", "wk", t("target_finish")));
  if (p.finishDate) {
    fin.appendChild(el("h4", "", formatDate(p.finishDate)));
    fin.appendChild(el("div", "shift", p.originalFinishDate && p.originalFinishDate !== p.finishDate
      ? t("moved_from", { d: formatDate(p.originalFinishDate) })
      : ""));
  } else {
    fin.appendChild(el("h4", "", t("weeks_approx", { n: p.weeksTotal || 0 })));
    fin.appendChild(el("div", "shift", t("finish_pending")));
  }
  list.appendChild(fin);
  return list;
}

function milestoneList(items) {
  const ul = el("ul", "ms-list");
  items.forEach((m) => {
    const li = el("li", `ms${m.done ? " done" : ""}`);
    li.appendChild(el("span", "diam"));
    li.appendChild(el("span", "", m.title || ""));
    if (m.done) li.appendChild(el("span", "ms-flag", t("milestone_done")));
    if (m.targetDate) li.appendChild(el("time", "", formatDate(m.targetDate)));
    ul.appendChild(li);
  });
  return ul;
}

// ── One task. A series, not a checkbox. ──
function todoRow(td, i, iso) {
  const planned = Math.max(0, Number(td.plannedCount) || 0);
  const completed = Math.max(0, Number(td.completedCount) || 0);
  const evs = eventsForTodo(td.id);
  const total = planned || evs.length;
  const allDone = total > 0 && completed >= total;
  const status = td.status || (allDone ? "done" : "pending");
  const onToday = evs.find((e) => e.date === iso && e.status !== "done" && e.status !== "skipped");
  const next = nextOpenEvent(td.id);

  const row = el("div", `todo series${allDone ? " done" : ""}${onToday ? " is-today" : ""}`);
  delay(row, i);

  // The control column: a plus that logs exactly one session, or an outline
  // that holds the column so the list still reads as one row per task.
  const canLog = !!next && !allDone && status !== "skipped";
  const check = el("span", `check${canLog ? "" : " idle"}`);
  if (canLog) {
    const btn = el("button", {
      class: "log-btn",
      title: t("log_session"),
      attrs: { type: "button", "aria-label": t("aria_log_session", { title: td.title || "" }) },
    });
    btn.appendChild(icon(ICONS.plus, 13, { weight: 3 }));
    btn.addEventListener("click", () => handlers.onComplete && handlers.onComplete(td.id, next.id));
    check.appendChild(btn);
  } else {
    const box = el("span", "box");
    if (allDone) box.appendChild(icon(ICONS.check, 13, { weight: 3.2 }));
    check.appendChild(box);
  }
  row.appendChild(check);

  const body = el("div", "todo-body");
  body.appendChild(el("span", "t", td.title || ""));

  const meta = el("div", "todo-meta");
  if (td.priority) {
    // Priority as weight, not as colour — colour is reserved for state.
    meta.appendChild(el("span", {
      class: "weight",
      attrs: { role: "img", "data-w": td.priority, "aria-label": tg("prio", td.priority) },
      children: [el("i"), el("i"), el("i")],
    }));
  }
  if (Number.isFinite(td.durationMin) && td.durationMin > 0) {
    meta.appendChild(el("span", "", `${td.durationMin} ${t("min")}`));
  }
  if (td.frequency) meta.appendChild(el("span", "", tg("freq", td.frequency)));
  meta.appendChild(el("span", { class: "td-state", text: tg("tstatus", status), attrs: { "data-s": status } }));
  if (td.resourceRef) meta.appendChild(el("span", "ph", t("uses_resource", { name: td.resourceRef })));
  else if (td.phase) meta.appendChild(el("span", "ph", td.phase));
  body.appendChild(meta);

  const sched = el("div", "todo-sched");
  if (total === 0) {
    // plannedCount is 0 — there is no series to count, and "0 of 1" would
    // invent one. Which reason applies is the difference between "not yet"
    // and "it didn't fit": once the plan has a calendar, a todo with no
    // sessions is one the weekly budget could not accommodate.
    const dropped = state.events.length > 0;
    sched.appendChild(el("span", `series-none${dropped ? " dropped" : ""}`,
      t(dropped ? "series_dropped" : "series_unscheduled")));
  } else {
    if (onToday) sched.appendChild(el("span", "tag-today", t("today_tag")));
    else if (next) sched.appendChild(el("span", "", t("next_on", { d: formatDate(next.date) })));
    else sched.appendChild(el("span", ""));

    const label = t("sess_done_of", { d: completed, n: total });
    const bar = el("span", { class: "series-bar", attrs: { role: "img", "aria-label": label } });
    const fill = el("i");
    fill.style.width = `${Math.min(100, (completed / total) * 100)}%`;
    bar.appendChild(fill);
    sched.appendChild(bar);
    sched.appendChild(el("span", "todo-prog", label));
  }
  body.appendChild(sched);
  row.appendChild(body);
  return row;
}

// ── History: why the finish date moved ──
// The answer to that question without asking the AI again.
function history(p) {
  const log = Array.isArray(p.changeLog) ? p.changeLog : [];
  const wrap = el("details", "history");
  wrap.appendChild(el("summary", "", `${t("plan_section_history")} · ${log.length}`));
  if (!log.length) {
    wrap.appendChild(el("p", "muted", t("history_empty")));
    return wrap;
  }
  const ol = el("ol", "hist");
  // Newest first: the most recent change is the one being asked about.
  log.slice().reverse().forEach((rec) => {
    const li = el("li", "hist-item");
    const head = el("div", "hist-head");
    if (rec.type) head.appendChild(el("span", "hist-type", rec.type));
    if (rec.at) head.appendChild(el("time", "hist-at", formatInstant(rec.at, planTimezone(), locale())));
    li.appendChild(head);
    if (rec.summary) li.appendChild(el("p", "hist-sum", rec.summary));

    const lines = [];
    const dayList = (a) => (Array.isArray(a) && a.length ? a.map(dayCodeLabel).join(" ") : "—");
    if ((Array.isArray(rec.oldDays) && rec.oldDays.length) || (Array.isArray(rec.newDays) && rec.newDays.length)) {
      lines.push(t("hist_days", { a: dayList(rec.oldDays), b: dayList(rec.newDays) }));
    }
    if (rec.oldWeeklyMinutes || rec.newWeeklyMinutes) {
      lines.push(t("hist_minutes", { a: rec.oldWeeklyMinutes || 0, b: rec.newWeeklyMinutes || 0 }));
    }
    if (rec.oldDeadline || rec.newDeadline) {
      lines.push(t("hist_deadline", {
        a: rec.oldDeadline ? formatDate(rec.oldDeadline) : "—",
        b: rec.newDeadline ? formatDate(rec.newDeadline) : "—",
      }));
    }
    if (rec.oldFinishDate || rec.newFinishDate) {
      lines.push(t("hist_finish", {
        a: rec.oldFinishDate ? formatDate(rec.oldFinishDate) : "—",
        b: rec.newFinishDate ? formatDate(rec.newFinishDate) : "—",
      }));
    }
    if (Number.isFinite(rec.finishShiftDays) && rec.finishShiftDays !== 0) {
      lines.push(rec.finishShiftDays > 0
        ? t("hist_shift_later", { n: rec.finishShiftDays })
        : t("hist_shift_earlier", { n: Math.abs(rec.finishShiftDays) }));
    }
    if (rec.rescheduled) {
      lines.push(t("hist_resched", { n: rec.futureSessions || 0, k: rec.completedPreserved || 0 }));
    }
    if (lines.length) {
      const ul = el("ul", "hist-lines");
      lines.forEach((s) => ul.appendChild(el("li", "", s)));
      li.appendChild(ul);
    }
    ol.appendChild(li);
  });
  wrap.appendChild(ol);
  return wrap;
}

// ═══ Kit tab ════════════════════════════════════════════════════════════
// What to get, with the point where the budget runs out said plainly — then
// what the learner is actually using.
export function renderKit(p) {
  const items = Array.isArray(p.setupItems) ? p.setupItems : [];
  const resources = Array.isArray(p.resources) ? p.resources : [];
  const box = clear($("kitContent"));
  $("kitEmpty").hidden = !!(items.length || resources.length);
  if (!items.length && !resources.length) return;

  if (items.length) {
    const priced = items.map((s) => {
      const n = String(s.priceRange || "").match(/\d+/g) || ["0"];
      return { ...s, lo: parseInt(n[0], 10) || 0, hi: parseInt(n[n.length - 1], 10) || 0 };
    });
    const grid = el("div", "kit-grid");
    priced.forEach((s, i) => {
      const card = el("article", "setup-card");
      delay(card, i, 0.05);
      card.appendChild(el("div", "setup-rank", String(i + 1)));
      const main = el("div", "setup-main");
      const row = el("div", "setup-row");
      row.appendChild(el("span", "setup-name", s.name || ""));
      if (s.owned) row.appendChild(el("span", "setup-over", t("owned")));
      row.appendChild(el("span", "setup-price", s.priceRange || t("price_free")));
      main.appendChild(row);
      if (s.category) main.appendChild(el("div", "setup-cat", s.category));
      if (s.rationale) main.appendChild(el("div", "setup-why", s.rationale));
      const links = shopLinks(s.links);
      if (links) main.appendChild(links);
      card.appendChild(main);
      grid.appendChild(card);
    });

    const lo = priced.reduce((n, s) => n + s.lo, 0);
    const hi = priced.reduce((n, s) => n + s.hi, 0);
    const free = priced.filter((s) => s.hi === 0).length;
    const foot = el("div", "kit-foot");
    foot.appendChild(numbers(t("kit_free", { n: free, m: priced.length })));
    foot.appendChild(numbers(t("kit_total", { lo, hi })));
    grid.appendChild(foot);
    box.appendChild(grid);
  }

  if (resources.length) {
    box.appendChild(sectionLabel(t("plan_section_resources")));
    const wrap = el("div", "resources");
    resources.forEach((r, i) => wrap.appendChild(resourceCard(r, i)));
    box.appendChild(wrap);
  }
}

// The counts read as numbers, so the numerals are set apart.
function numbers(text) {
  const span = el("span");
  String(text).split(/(\d+)/).forEach((part, i) => {
    if (i % 2 === 1) span.appendChild(el("b", "", part));
    else span.appendChild(document.createTextNode(part));
  });
  return span;
}

function resourceCard(r, i) {
  const inUse = String(r.selected || r.recommended || "").trim();
  const swapped = !!r.recommended && !!r.selected && r.recommended !== r.selected;
  const card = el("article", `res${swapped ? " swapped" : ""}`);
  delay(card, i, 0.05);

  if (r.need) {
    const need = el("div", "res-need");
    need.appendChild(el("span", "res-need-k", t("res_for")));
    need.appendChild(document.createTextNode(r.need));
    card.appendChild(need);
  }

  const main = el("div", "res-main");
  main.appendChild(el("span", "res-tag", t("res_in_use")));
  main.appendChild(el("span", "res-name", inUse));
  if (r.kind) main.appendChild(el("span", "res-kind", r.kind));
  card.appendChild(main);

  // A learner who bought a different book sees the plan tracking reality.
  if (swapped) card.appendChild(el("s", "res-was", t("res_was", { name: r.recommended })));

  const sub = el("div", "res-sub");
  if (r.source) {
    sub.appendChild(el("span", "", r.source === "user" ? t("res_source_user")
      : r.source === "recommended" ? t("res_source_recommended") : r.source));
  }
  if (swapped && r.equivalent === false) sub.appendChild(el("span", "res-warn", t("res_not_equivalent")));
  if (Array.isArray(r.rejected) && r.rejected.length) {
    sub.appendChild(el("span", "", t("res_rejected", { list: r.rejected.join(", ") })));
  }
  if (sub.childNodes.length) card.appendChild(sub);
  if (r.note) card.appendChild(el("p", "res-note", r.note));
  const links = shopLinks(r.links);
  if (links) card.appendChild(links);
  return card;
}

// Shop links open the shop's own search for the item. The backend marks them
// kind "search": they are where to look, not a product whose price was checked,
// so the label says "search" and nothing here shows a price.
function shopLinks(links) {
  const list = (Array.isArray(links) ? links : []).filter((l) => /^https:\/\//.test(String(l && l.url)));
  if (!list.length) return null;
  const row = el("div", { class: "shop-links", attrs: { "aria-label": t("shop_search_in") } });
  row.appendChild(el("span", "shop-k", t("shop_search_in")));
  list.forEach((l) => {
    row.appendChild(el("a", {
      class: "shop-link",
      text: l.label || l.provider,
      attrs: { href: l.url, target: "_blank", rel: "noopener noreferrer" },
    }));
  });
  return row;
}

// ═══ Goal band ══════════════════════════════════════════════════════════
// The promise, on every tab: what this is, and where you are in it.
export function renderGoalBand(p) {
  const f = planFacts(p);
  $("gbTitle").textContent = p.skill || "";
  $("gbTrack").textContent = f.track;

  const chips = clear($("gbChips"));
  const chip = (label, value, mono) => {
    if (value == null || value === "") return;
    const c = el("span", "gb-chip");
    c.appendChild(el("u", "", label));
    c.appendChild(el("span", mono ? "v" : "", String(value)));
    chips.appendChild(c);
  };
  if (f.level) chip(t("chip_level"), tg("lvl", f.level));
  if (f.weeklyMinutes) {
    chip(t("chip_weekly"), f.weeklyMinutes % 60 === 0
      ? t("hours_n", { n: f.weeklyMinutes / 60 })
      : duration(f.weeklyMinutes), true);
  } else if (f.hoursPerWeek) {
    chip(t("chip_weekly"), t("hours_n", { n: f.hoursPerWeek }), true);
  }
  if (f.days && f.days.length) chip(t("chip_days"), f.days.map(dayCodeLabel).join(" "));
  if (f.weeks) chip(t("chip_span"), t("weeks_n", { n: f.weeks }), true);
  // Sessions come from the server's calendar once it exists.
  if (state.events.length && f.weeks) {
    chip(t("chip_load"), t("per_week", { n: Math.max(1, Math.round(state.events.length / f.weeks)) }), true);
  }
  if (p.deadline) chip(t("deadline"), formatDate(p.deadline), true);

  updateRail(p, false);
}

// start → today → finish
export function updateRail(p, shifted) {
  $("goalBand").classList.toggle("shifted", !!shifted);

  const start = p.startDate;
  const finish = p.finishDate;
  const iso = today();

  railLabel("lblStart", t("lbl_started"), start ? formatDate(start) : "—");
  railLabel("lblNow", t("lbl_today"), formatDate(iso));
  railLabel("lblEnd", t("lbl_finish"), finish ? formatDate(finish) : t("weeks_approx", { n: p.weeksTotal || 0 }));

  const span = start && finish ? daysBetween(start, finish) : 0;
  const pct = span > 0 ? Math.max(0, Math.min(1, daysBetween(start, iso) / span)) : 0;
  const left = (pct * 100).toFixed(1) + "%";
  $("railFill").style.width = left;
  $("tickNow").style.left = left;
  $("lblNow").style.left = left;

  // Only mark "today" while today is actually on the rail, and only label it
  // where the label will not sit on top of Started or Finish.
  const onRail = !!(start && finish && iso >= start && iso <= finish);
  $("tickNow").hidden = !onRail;
  $("lblNow").hidden = !onRail || pct < 0.09 || pct > 0.91;

  // "0 of 0 sessions done" is not a progress report. Until the plan has a
  // calendar there is nothing to count, so say that instead.
  $("gbProgress").textContent = start && state.events.length
    ? t("gb_progress", { w: weekIndex(p), t: p.weeksTotal || 1, d: doneCount(), n: state.events.length })
    : t("gb_unscheduled");

  const slip = (p.originalFinishDate && finish && p.originalFinishDate !== finish)
    ? daysBetween(p.originalFinishDate, finish) : 0;
  const slipEl = $("gbSlip");
  slipEl.classList.toggle("moved", slip > 0);
  slipEl.textContent = start ? (slip > 0 ? t("gb_moved", { d: slip }) : t("gb_ontrack")) : "";

  updatePlanState(p, slip);
}

function railLabel(id, label, value) {
  const node = clear($(id));
  node.appendChild(document.createTextNode(label));
  node.appendChild(el("b", "", value));
}

// The header shows the learner's state, not the gateway's.
function updatePlanState(p, slip) {
  const wrap = $("planState");
  const dot = $("psDot");
  wrap.hidden = false;
  if (!p || !p.startDate) {
    dot.className = "ps-dot slipped";
    $("psText").textContent = t("ps_planning");
    return;
  }
  const behind = slip > 0;
  dot.className = "ps-dot" + (behind ? " slipped" : "");
  $("psText").textContent = behind
    ? t("ps_week_slip", { w: weekIndex(p), t: p.weeksTotal || 1, d: slip })
    : t("ps_week", { w: weekIndex(p), t: p.weeksTotal || 1 });
}

function weekIndex(p) {
  if (!p.startDate) return 1;
  const n = Math.floor(daysBetween(p.startDate, today()) / 7) + 1;
  return Math.max(1, Math.min(n, p.weeksTotal || 1));
}
