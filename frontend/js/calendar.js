// ═══ The schedule, as a week or a month ══════════════════════════════════
// Everything in here obeys one rule, enforced by only ever using dates.js:
//
//   `date` and `startTime` are FLOATING values belonging to plan.timezone.
//   They are rendered as the strings they are. No `new Date(event.date)`, no
//   reformatting through the browser's zone, no exceptions. Weekday names,
//   month names and day arithmetic all come from integer maths and our own
//   name tables.
//
// The preview matters too: POST /api/schedule produces `proposed` events, and
// they are drawn as a proposal — dashed and unsettled — until confirm
// promotes them. Nothing here confirms on its own.

import { $, el, clear, icon, ICONS } from "./dom.js";
import { t, tg, formatDate, weekdayName, monthName, monthTitle, duration } from "./i18n.js";
import {
  addDays, addMonths, mondayOf, weekDates, monthGrid, sameMonth, firstOfMonth,
  parseDate, todayInZone, formatTime, endTime, weekdayCode, normalizeDayCode,
  browserTimezone,
} from "./dates.js";
import { state, planTimezone } from "./state.js";

let handlers = {};

export function initCalendar(h) {
  handlers = h || {};
  $("viewSeg").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-view]");
    if (!b || b.dataset.view === state.calView) return;
    state.calView = b.dataset.view;
    render();
  });
  $("wkPrev").addEventListener("click", () => shift(-1));
  $("wkNext").addEventListener("click", () => shift(1));
  $("wkToday").addEventListener("click", () => {
    state.anchor = null;
    render();
  });
}

// "Today" is decided in the plan's timezone, which the backend owns.
function today() {
  return todayInZone(planTimezone());
}

// ── Where the view sits ──
// "This week" is only the right answer when this week actually has sessions
// in it. A plan whose first phase starts in three weeks would otherwise open
// on an empty grid right after scheduling, which reads as a broken calendar
// rather than an accurate one.
function anchor() {
  if (state.anchor) return state.anchor;
  const iso = today();
  const dates = state.events.map((e) => e && e.date).filter(Boolean).sort();
  if (!dates.length) {
    state.anchor = (state.plan && state.plan.startDate) || iso;
    return state.anchor;
  }
  const from = mondayOf(iso);
  const to = addDays(from, 6);
  const thisWeek = dates.some((d) => d >= from && d <= to);
  state.anchor = thisWeek ? iso : (dates.find((d) => d >= iso) || dates[dates.length - 1]);
  return state.anchor;
}
export function resetAnchor() {
  state.anchor = null;
}

function shift(dir) {
  state.anchor = state.calView === "month"
    ? addMonths(anchor(), dir)
    : addDays(anchor(), dir * 7);
  render();
}

// plan.days is the learner's availability. Sessions only ever land on those
// days, and showing the rest differently is what makes that visible.
function studyDays() {
  const days = (state.plan && Array.isArray(state.plan.days) ? state.plan.days : [])
    .map(normalizeDayCode)
    .filter(Boolean);
  return new Set(days);
}

function bounds() {
  const dates = state.events.map((e) => e && e.date).filter(Boolean).sort();
  return {
    start: (state.plan && state.plan.startDate) || dates[0] || null,
    end: (state.plan && state.plan.finishDate) || dates[dates.length - 1] || null,
  };
}

function eventsByDate() {
  const map = new Map();
  state.events.forEach((e) => {
    if (!e || !parseDate(e.date)) return;
    if (!map.has(e.date)) map.set(e.date, []);
    map.get(e.date).push(e);
  });
  // Within a day, order by the clock face — as strings, which sort correctly
  // for zero-padded HH:MM.
  map.forEach((list) => list.sort((a, b) => String(a.startTime).localeCompare(String(b.startTime))));
  return map;
}

// ── Entry point ──
export function render() {
  const has = state.events.length > 0;
  $("calEmpty").hidden = has;
  $("calContent").hidden = !has;
  if (!has) return;

  $("viewSeg").querySelectorAll("button[data-view]").forEach((b) => {
    const on = b.dataset.view === state.calView;
    b.classList.toggle("active", on);
    b.setAttribute("aria-pressed", String(on));
  });

  const month = state.calView === "month";
  $("wkGrid").hidden = month;
  $("moGrid").hidden = !month;
  if (month) renderMonth(); else renderWeek();

  const tz = planTimezone();
  const device = browserTimezone();
  $("wkZone").textContent = tz && device && tz !== device
    ? t("zone_note_differs", { tz, device })
    : t("zone_note", { tz });
}

// ── Week: the hour grid ──
function renderWeek() {
  const weekStart = mondayOf(anchor());
  const days = weekDates(weekStart);
  const iso = today();
  const codes = studyDays();
  const { start: planStart, end: planEnd } = bounds();
  const outOfPlan = (d) => (planStart && d < planStart) || (planEnd && d > planEnd);

  const inWeek = state.events.filter((e) => e.date >= days[0] && e.date <= days[6]);
  const busy = new Set(inWeek.map((e) => e.date));
  // Days before the plan starts (or after it ends) are not free time — they
  // are not part of the plan at all, and are not offered as buffer.
  const dayClass = (d) => (outOfPlan(d) ? " out" : busy.has(d) ? "" : " free");

  const dates = state.events.map((e) => e.date).sort();
  const firstWeek = mondayOf(planStart || dates[0]);
  const lastWeek = mondayOf(dates[dates.length - 1]);
  $("wkPrev").disabled = firstWeek >= weekStart;
  $("wkNext").disabled = lastWeek <= weekStart;
  // "Today" is a shortcut back, so it is dead weight when you are already there.
  $("wkToday").disabled = clampWeek(mondayOf(iso), firstWeek, lastWeek) === weekStart;
  $("wkRange").textContent = weekRange(days);

  // Rows: only the hours this week actually uses.
  const hours = [...new Set(inWeek.map((e) => String(e.startTime || "").slice(0, 2)))].sort();
  const byCell = {};
  inWeek.forEach((e) => {
    const key = e.date + "@" + String(e.startTime || "").slice(0, 2);
    (byCell[key] = byCell[key] || []).push(e);
  });
  Object.values(byCell).forEach((list) => list.sort((a, b) => String(a.startTime).localeCompare(String(b.startTime))));

  const grid = clear($("wkGrid"));
  grid.appendChild(el("div", "wk-corner"));
  days.forEach((d) => {
    const p = parseDate(d);
    const h = el("div", `wk-h${d === iso ? " today" : ""}${dayClass(d)}`);
    h.appendChild(el("div", "dow", weekdayName(d, "short")));
    h.appendChild(el("div", "dnum", String(p ? p.d : "")));
    grid.appendChild(h);
  });

  hours.forEach((hh) => {
    grid.appendChild(el("div", "wk-hr", `${hh}:00`));
    days.forEach((d) => {
      const cell = el("div", `wk-cell${dayClass(d)}`);
      (byCell[d + "@" + hh] || []).forEach((e) => cell.appendChild(sessionCard(e, d)));
      grid.appendChild(cell);
    });
  });

  const mins = (pred) => inWeek.filter(pred).reduce((n, e) => n + (e.durationMin || 0), 0);
  loadLine(t("wk_load", {
    s: duration(mins(() => true)),
    d: duration(mins((e) => e.status === "done")),
  }));

  // Which days this week are clear, said plainly.
  const clearDays = days.filter((d) => !busy.has(d) && !outOfPlan(d));
  const offDays = clearDays.filter((d) => codes.size === 0 || !codes.has(weekdayCode(d)));
  const startsHere = planStart && planStart > days[0] && planStart <= days[6];
  const names = (list) => list.map((d) => weekdayName(d, "short")).join(", ");
  $("wkHint").textContent = clearDays.length
    ? t(clearDays.length === 1 ? "wk_free_one" : "wk_free_many", { days: names(offDays.length ? offDays : clearDays) })
    : startsHere ? t("wk_starts", { d: formatDate(planStart) })
      : days.every((d) => !outOfPlan(d)) ? t("wk_full") : "";
}

// The numerals in the load line are set apart, like every other count.
function loadLine(text) {
  const host = clear($("wkLoad"));
  String(text).split(/(\d+(?:[.,]\d+)?)/).forEach((part, i) => {
    if (i % 2 === 1) host.appendChild(el("b", "", part));
    else host.appendChild(document.createTextNode(part));
  });
}

function weekRange(days) {
  const a = parseDate(days[0]);
  const b = parseDate(days[6]);
  if (!a || !b) return "";
  // The month is named once when it does not change across the week.
  if (a.y === b.y && a.m === b.m) return `${a.d}–${b.d} ${monthName(a.m, true)} ${a.y}`;
  if (a.y === b.y) return `${a.d} ${monthName(a.m, true)} – ${b.d} ${monthName(b.m, true)} ${a.y}`;
  return `${formatDate(days[0])} – ${formatDate(days[6])}`;
}

function clampWeek(w, lo, hi) {
  return w < lo ? lo : w > hi ? hi : w;
}

// ── One session ──
const STATUS_CLASS = {
  done: " done",
  rolled_over: " missed",
  skipped: " missed",
  proposed: " proposed",
};

function sessionCard(e, d) {
  const moved = Number.isFinite(e.rolledOver) && e.rolledOver > 0;
  const card = el("div", "sess" + (STATUS_CLASS[e.status] || (moved ? " moved" : "")));

  // The grid is visual; spell the same facts out for anyone not seeing it.
  const parts = [e.title, `${weekdayName(d, "short")} ${formatDate(d)}`, formatTime(e.startTime)];
  if (e.durationMin) parts.push(duration(e.durationMin));
  parts.push(tg("status", e.status));
  if (moved) parts.push(t("rolled_n", { n: e.rolledOver }));
  if (Number.isFinite(e.reminderMin) && e.reminderMin > 0) parts.push(t("reminder", { n: e.reminderMin }));
  const label = parts.join(", ");
  card.title = label;
  card.setAttribute("role", "img");
  card.setAttribute("aria-label", label);

  card.appendChild(el("b", "", e.title || ""));
  const end = endTime(e.startTime, e.durationMin);
  card.appendChild(el("span", "dur", end
    ? `${formatTime(e.startTime)}–${end.time}${end.nextDay ? " +1" : ""}`
    : `${formatTime(e.startTime)} · ${tg("status", e.status)}`));
  if (moved) card.appendChild(el("span", "moved-n", t("rolled_n", { n: e.rolledOver })));

  // Passing eventId completes THIS session rather than the soonest one.
  if (e.status !== "done" && e.status !== "skipped" && e.todoId) {
    const b = el("button", {
      class: "sess-log",
      title: t("log_session"),
      attrs: { type: "button", "aria-label": t("aria_log_session", { title: e.title || "" }) },
    });
    b.appendChild(icon(ICONS.plus, 11, { weight: 3 }));
    b.addEventListener("click", (ev) => {
      ev.stopPropagation();
      handlers.onComplete && handlers.onComplete(e.todoId, e.id);
    });
    card.appendChild(b);
  }
  return card;
}

// ── Month ──
function renderMonth() {
  const at = anchor();
  const cells = monthGrid(firstOfMonth(at));
  const byDate = eventsByDate();
  const iso = today();
  const codes = studyDays();
  const { start: planStart, end: planEnd } = bounds();
  const outOfPlan = (d) => (planStart && d < planStart) || (planEnd && d > planEnd);

  const dates = state.events.map((e) => e.date).sort();
  const thisMonth = firstOfMonth(at);
  $("wkPrev").disabled = firstOfMonth(planStart || dates[0]) >= thisMonth;
  $("wkNext").disabled = firstOfMonth(dates[dates.length - 1]) <= thisMonth;
  $("wkToday").disabled = firstOfMonth(iso) === thisMonth;
  $("wkRange").textContent = monthTitle(at);

  const host = clear($("moGrid"));
  const head = el("div", "mo-head");
  weekDates(mondayOf(cells[0])).forEach((d) => head.appendChild(el("span", "mo-dow", weekdayName(d, "short"))));
  host.appendChild(head);

  const grid = el("div", "mo-grid");
  let shown = 0;
  cells.forEach((d) => {
    const p = parseDate(d);
    const list = byDate.get(d) || [];
    const inMonth = sameMonth(d, at);
    if (inMonth) shown += list.length;
    const off = codes.size > 0 && !codes.has(weekdayCode(d));

    const cell = el("button", {
      class: ["mo-cell", inMonth ? "" : "outside",
        outOfPlan(d) ? "out" : (off && !list.length ? "free" : ""),
        d === iso ? "today" : ""].filter(Boolean).join(" "),
      attrs: { type: "button", "aria-label": `${formatDate(d)}${list.length ? ` · ${list.length}` : ""}` },
    });
    // A month cell is a way into the week that contains it.
    cell.addEventListener("click", () => {
      state.anchor = d;
      state.calView = "week";
      render();
    });

    cell.appendChild(el("span", "mo-num", String(p ? p.d : "")));
    const pills = el("div", "mo-pills");
    list.slice(0, 3).forEach((e) => {
      const moved = Number.isFinite(e.rolledOver) && e.rolledOver > 0;
      const kind = e.status === "done" ? " done"
        : (e.status === "rolled_over" || e.status === "skipped") ? " missed"
          : e.status === "proposed" ? " proposed" : (moved ? " moved" : "");
      const pill = el("span", "mo-pill" + kind);
      pill.appendChild(el("b", "", formatTime(e.startTime)));
      pill.appendChild(el("span", "", e.title || ""));
      pills.appendChild(pill);
    });
    if (list.length > 3) pills.appendChild(el("span", "mo-more", `+${list.length - 3}`));
    cell.appendChild(pills);
    grid.appendChild(cell);
  });
  host.appendChild(grid);

  const monthEvents = state.events.filter((e) => sameMonth(e.date, at));
  loadLine(t("wk_load", {
    s: duration(monthEvents.reduce((n, e) => n + (e.durationMin || 0), 0)),
    d: duration(monthEvents.filter((e) => e.status === "done").reduce((n, e) => n + (e.durationMin || 0), 0)),
  }));
  $("wkHint").textContent = shown ? "" : t("no_sessions_month");
}
