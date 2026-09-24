// ═══ start.ai — application entry ════════════════════════════════════════
// Plain ES modules, no build step. The Go backend serves this folder, so
// every request is same-origin and there is no CORS story.
//
// The shape of the whole thing:
//   · One endpoint carries the entire relationship. /api/chat handles the
//     scope check, the interview, the recap gate, and — after the plan
//     exists — every question and every "I can't study Tuesdays anymore".
//     So the composer is never disabled on success: plan_ready is a
//     beginning, not an end.
//   · The server owns the truth. planChanged/scheduleChanged say what moved;
//     we refetch that rather than patching a local copy.
//   · Floating dates stay floating. See js/dates.js.

import {
  createSession, chat, getPlan, schedule as apiSchedule, confirmSchedule,
  getCalendar, completeTodo, rollover as apiRollover, meter as apiMeter,
  hasToken, clearToken, ApiError, NETWORK,
} from "./js/api.js";
import { lang, setLang, t, LANGS } from "./js/i18n.js";
import {
  state, loadState, saveState, writeState, startGoal, switchGoal, pushMessage,
  findMessage, titleGoal, activeGoal, resetPlanState, proposedCount,
} from "./js/state.js";
import { $, clear, toast, applyTheme, themePref, syncThemeButton, announce } from "./js/dom.js";
import {
  initChat, renderConversation, startTyping, stopTyping, syncComposer, focusComposer,
  setDock, dockIsOpen, syncDock, scrollToEnd,
} from "./js/chat.js";
import { initPlan, renderPlan, renderKit, renderGoalBand, updateRail, setAnimateRows } from "./js/plan.js";
import { initCalendar, render as renderCalendar, resetAnchor } from "./js/calendar.js";
import {
  initSidebar, renderChatList, setSidebar, syncAccount, syncSettings,
  closeSettings, settingsOpen, closeChatMenu, chatMenuOpen, acctMenuOpen, closeAcctMenu,
  confirmPending, hideConfirm, flush as flushSidebar,
} from "./js/sidebar.js";
import { downloadIcs } from "./js/ics.js";
import { browserTimezone } from "./js/dates.js";

const DEV = new URLSearchParams(location.search).has("dev");
const TAB_INDEX = { plan: 0, cal: 1, kit: 2 };
let planShownOnce = false;

// ── Chrome in the chosen language ───────────────────────────────────────
function applyI18n() {
  document.querySelectorAll("[data-i18n]").forEach((n) => { n.textContent = t(n.dataset.i18n); });
  document.querySelectorAll("[data-i18n-ph]").forEach((n) => { n.placeholder = t(n.dataset.i18nPh); });
  // Controls that show only an icon carry their label in the current language.
  document.querySelectorAll("[data-i18n-aria]").forEach((n) => { n.setAttribute("aria-label", t(n.dataset.i18nAria)); });
  document.documentElement.lang = lang;

  syncGreeting();
  syncLangButtons();
  syncThemeButton();
  syncComposer();
  syncDock();
  syncAccount();
  syncSettings();          // a no-op while the sheet is closed
  syncToolbar();
}

// The opening question uses your name when it knows it.
function syncGreeting() {
  const h = document.querySelector("#intakeHead h1");
  if (!h) return;
  const name = (state.name || "").trim();
  h.textContent = name ? t("intake_h_named", { name }) : t("intake_h");
}

function syncLangButtons() {
  document.querySelectorAll("#langSwitch button[data-lang]").forEach((b) => {
    const on = b.dataset.lang === lang;
    b.classList.toggle("active", on);
    b.setAttribute("aria-pressed", String(on));
  });
}

function changeLang(next) {
  if (!LANGS.includes(next) || next === lang) return;
  setLang(next);
  // The backend is told on the next /api/chat call, which always carries
  // `lang`. Everything on screen is chrome, so it re-renders now.
  applyI18n();
  renderChatList();
  renderConversation({ scroll: false });
  if (state.plan) renderPanel({ animate: false });
}

// ── Stage ───────────────────────────────────────────────────────────────
// Two stages, one surface. During intake the conversation owns a centred
// column; once a plan exists it collapses to a dock and the plan takes over.
function setStage(stage) {
  document.body.dataset.stage = stage;
  const plan = stage === "plan";
  $("intakeHead").hidden = plan;
  $("goalBand").hidden = !plan;
  $("surface").hidden = !plan;
  if (!plan) $("planState").hidden = true;
  syncComposer();
  syncDock();
}

function switchTab(name) {
  const tab = TAB_INDEX[name] == null ? "plan" : name;
  state.tab = tab;
  $("seg").style.setProperty("--i", String(TAB_INDEX[tab]));
  document.querySelectorAll(".seg-btn").forEach((b) => {
    const on = b.dataset.tab === tab;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
    b.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll(".pane").forEach((p) => {
    p.classList.toggle("active", p.id === `pane-${tab}`);
  });
}

// ── Boot ────────────────────────────────────────────────────────────────
async function boot() {
  loadState();
  applyTheme(themePref());
  setLang(lang);

  initChat({ onSend: send, onRetry: retry, onViewPlan: () => setDock(false) });
  initPlan({ onComplete: complete });
  initCalendar({ onComplete: complete });
  initSidebar({
    onNewChat: () => newGoal(),
    onOpenChat: openChat,
    onChatRemoved: () => newGoal(),
    onNameChange: syncGreeting,
    onLangChange: changeLang,
    onReset: resetEverything,
  });

  wireChrome();
  setSidebar(state.sidebarOpen);
  setStage("intake");
  switchTab("plan");
  applyI18n();
  renderChatList();

  // A reload must not start a new conversation: /api/session with a token
  // deliberately opens a fresh one, which would abandon the interview.
  if (hasToken() && state.sessionId && state.messages.length) {
    renderConversation();
    if (state.planId) {
      try {
        await loadPlan(state.planId);
        // Reopening a finished plan should show the plan, not the transcript
        // over the top of it. The dock is one click away.
        setDock(false);
      } catch (e) {
        await handlePanelError(e);
      }
    }
  } else {
    await newGoal();
  }

  checkMeter();
  focusComposer();
  window.addEventListener("beforeunload", () => { flushSidebar(); writeState(); });
}

function wireChrome() {
  $("themeToggle").addEventListener("click", () => {
    applyTheme(document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark");
    syncSettings();
  });
  $("langSwitch").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-lang]");
    if (b) changeLang(b.dataset.lang);
  });
  $("seg").addEventListener("click", (e) => {
    const b = e.target.closest(".seg-btn");
    if (b) switchTab(b.dataset.tab);
  });
  // Arrow keys move between tabs, as a tablist should.
  $("seg").addEventListener("keydown", (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    const order = ["plan", "cal", "kit"];
    const i = order.indexOf(state.tab);
    const next = order[(i + (e.key === "ArrowRight" ? 1 : order.length - 1)) % order.length];
    e.preventDefault();
    switchTab(next);
    $(`tab-${next}`).focus();
  });

  $("scheduleBtn").addEventListener("click", doSchedule);
  $("emptyScheduleBtn").addEventListener("click", doSchedule);
  $("confirmBtn").addEventListener("click", doConfirm);
  $("icsBtn").addEventListener("click", doIcs);
  $("rolloverBtn").addEventListener("click", doRollover);

  if (DEV) {
    $("meter").hidden = false;
    $("rolloverBtn").hidden = false;
  }

  // "System" keeps following the OS after the choice, so listen for a switch.
  const mq = matchMedia("(prefers-color-scheme: dark)");
  const onScheme = () => { if (themePref() === "system") applyTheme("system"); };
  if (mq.addEventListener) mq.addEventListener("change", onScheme);
  else if (mq.addListener) mq.addListener(onScheme);

  window.addEventListener("resize", () => {
    // Coming back to a wide window should not leave the drawer stranded.
    if (window.innerWidth > 940) setSidebar(state.sidebarOpen);
    closeChatMenu();
  });

  wireKeyboard();
  wireOnScreenKeyboard();
}

// ── Sessions ────────────────────────────────────────────────────────────
// POST /api/session is the only public route. With no token it creates the
// user and hands one back exactly once; with a token it starts a NEW
// conversation for the same user, which is how a second goal begins.
async function newGoal() {
  try {
    const r = await createSession({
      name: (state.name || "").trim(),
      // The learner's real IANA zone. Everything the scheduler does is
      // anchored to it, so guessing here would misplace every session.
      timezone: browserTimezone(),
      lang,
    });
    state.userId = r.userId || state.userId;
    if (typeof r.timezone === "string" && r.timezone) state.timezone = r.timezone;
    startGoal({ sessionId: r.sessionId, lang });
    state.quotaBlocked = false;
    planShownOnce = false;
    // The opening greeting is a real assistant turn — it goes in the thread.
    if (r.assistant) pushMessage({ role: "assistant", text: r.assistant, stage: r.stage || "scope_check" });
    state.stage = r.stage || "scope_check";

    setStage("intake");
    setDock(true);
    switchTab("plan");
    $("planEmpty").hidden = false;
    $("planContent").hidden = true;
    clear($("kitContent"));
    $("kitEmpty").hidden = false;
    renderCalendar();
    syncToolbar();
    renderChatList();
    renderConversation();
    focusComposer();
  } catch (e) {
    const d = describe(e);
    // At boot, "is the server up?" is the more actionable version of offline.
    pushMessage({ role: "notice", text: d.code === NETWORK ? t("err_backend") : d.text });
    renderConversation();
  }
}

async function openChat(sessionId) {
  if (!switchGoal(sessionId)) return;
  renderChatList();
  renderConversation();
  if (state.planId) {
    try {
      await loadPlan(state.planId);
      setStage("plan");
      setDock(false);
    } catch (e) {
      await handlePanelError(e);
    }
  } else {
    setStage("intake");
    setDock(true);
    $("planEmpty").hidden = false;
    $("planContent").hidden = true;
    syncToolbar();
  }
  if (window.innerWidth <= 940) setSidebar(false);
  focusComposer();
}

async function resetEverything() {
  try {
    localStorage.removeItem("startai.state.v1");
    localStorage.removeItem("startai.lang");
    localStorage.removeItem("startai.theme");
    localStorage.removeItem("startai.sidebar");
  } catch (_) { /* best effort */ }
  clearToken();
  state.name = "";
  state.goals = [];
  state.sessionId = null;
  state.messages = [];
  state.options = [];
  state.progress = null;
  state.stage = "scope_check";
  resetPlanState();
  closeSettings();
  applyTheme("system");
  applyI18n();
  renderChatList();
  await newGoal();
  toast(t("done_reset"), { kind: "ok" });
}

// ── Conversation ────────────────────────────────────────────────────────
async function send(text) {
  const msg = String(text || "").trim();
  if (!msg || state.busy || state.quotaBlocked) return;
  if (!state.sessionId) { await newGoal(); if (!state.sessionId) return; }
  // Never answer into a closed drawer.
  if (!dockIsOpen()) setDock(true);
  const userMsg = pushMessage({ role: "user", text: msg });
  await deliver(userMsg);
}

// Retry resends the same message. The backend recorded it but produced no
// assistant reply, so the message stays where it is and only the delivery is
// attempted again.
async function retry(messageId) {
  const msg = findMessage(messageId);
  if (!msg || state.busy) return;
  msg.failed = null;
  await deliver(msg);
}

async function deliver(userMsg) {
  state.busy = true;
  renderConversation();
  // Approving the recap is what triggers plan generation, and that can take
  // most of the server's 55-second budget.
  startTyping({ planStage: state.stage === "confirm_plan" });

  try {
    const turn = await chat({ sessionId: state.sessionId, message: userMsg.text, lang });
    stopTyping();
    state.busy = false;
    userMsg.failed = null;
    await handleTurn(turn);
  } catch (e) {
    stopTyping();
    state.busy = false;
    await handleChatError(e, userMsg);
  }
  saveState();
  renderChatList();
  renderConversation();
  focusComposer();
}

// Switch on `stage`. Never parse the prose.
async function handleTurn(turn) {
  if (!turn || typeof turn !== "object") return;
  const stage = typeof turn.stage === "string" && turn.stage ? turn.stage : state.stage;

  // `options` may be null. Treat it as absent, not as an error.
  const options = Array.isArray(turn.options)
    ? turn.options.filter((s) => typeof s === "string" && s.trim())
    : [];

  // Name the conversation after the first message that was actually a goal.
  // "what's the weather" is a redirect, not a title for the list.
  if (stage !== "out_of_scope") {
    const firstUser = state.messages.find((m) => m.role === "user");
    if (firstUser) titleGoal(firstUser.text);
  }

  state.stage = stage;
  state.options = options;
  // `progress` is intake-only, and absent means "not applicable" — not zero.
  state.progress = stage === "intake" && turn.progress && typeof turn.progress === "object"
    ? turn.progress
    : null;

  const newPlan = !!turn.planId && turn.planId !== state.planId;
  const tags = [];
  if (stage === "plan_ready" && newPlan) tags.push("plan_ready");
  // A quiet inline marker on the bubble, not a modal.
  if (turn.planChanged && !newPlan) tags.push("plan_updated");
  if (turn.scheduleChanged && !newPlan) tags.push("schedule_updated");

  pushMessage({
    role: "assistant",
    text: turn.assistant || "",
    stage,
    question: typeof turn.question === "string" ? turn.question : "",
    options,
    planId: turn.planId || null,
    tags,
  });
  renderConversation();

  try {
    if (newPlan) {
      await loadPlan(turn.planId);
      setStage("plan");
      // The page just changed underneath the learner. Say so, and hand the
      // plan the working area instead of answering into a drawer.
      setDock(false);
      switchTab("plan");
      announce(t("plan_ready_sr"));
      $("gbTitle").focus();
      planShownOnce = true;
    } else if (turn.planChanged && state.planId) {
      // The server changed the plan; refetch it rather than guessing what.
      state.plan = await getPlan(state.planId);
      if (turn.scheduleChanged) await loadCalendar();
      renderPanel({ animate: false });
    } else if (turn.scheduleChanged && state.planId) {
      await loadCalendar();
      renderPanel({ animate: false });
    }
  } catch (e) {
    await handlePanelError(e);
  }
  saveState();
}

// ── Failure ─────────────────────────────────────────────────────────────
// The backend's `message` is written to be read by the user, so it is shown
// verbatim. `ref` is a support code and never stands in for the message.
function describe(e) {
  if (!(e instanceof ApiError)) return { text: t("err_generic_safe"), retryable: false, code: "", ref: null };
  const text = e.message && e.message.trim()
    ? e.message.trim()
    : e.code === NETWORK ? t("err_network") : t("err_generic_safe");
  return { text, retryable: !!e.retryable, code: e.code, ref: e.ref };
}

async function handleChatError(e, userMsg) {
  const d = describe(e);

  if (d.code === "UNAUTHORIZED") {
    // The token is dead. Clear it and start over as a new user — there is no
    // way to re-issue a token for the old one.
    clearToken();
    try { localStorage.removeItem("startai.state.v1"); } catch (_) { /* best effort */ }
    state.goals = [];
    state.sessionId = null;
    state.messages = [];
    resetPlanState();
    setStage("intake");
    await newGoal();
    pushMessage({ role: "notice", text: t("signed_out") });
    return;
  }

  if (d.code === "SESSION_NOT_FOUND") {
    // The conversation is gone from the server (a restart, a wiped data dir).
    // Open a fresh one and say so, rather than looping on a dead id.
    const text = userMsg.text;
    setStage("intake");
    await newGoal();
    pushMessage({ role: "notice", text: t("session_expired") });
    const again = pushMessage({ role: "user", text });
    again.failed = { code: d.code, message: "", ref: null, retryable: true };
    return;
  }

  if (d.code === "DAILY_QUOTA_EXCEEDED") {
    // Not retryable today. Say so and stop offering the composer.
    state.quotaBlocked = true;
    userMsg.failed = { code: d.code, message: d.text, ref: d.ref, retryable: false };
    toast(d.text, { ref: d.ref, timeout: 9000 });
    return;
  }

  // AI_UNAVAILABLE / AI_RATE_LIMITED / INTERNAL_ERROR / NETWORK are worth a
  // Retry; INVALID_REQUEST and friends are not.
  userMsg.failed = { code: d.code, message: d.text, ref: d.ref, retryable: d.retryable };
  if (!d.retryable) toast(d.text, { ref: d.ref });
}

async function handlePanelError(e) {
  const d = describe(e);
  if (d.code === "PLAN_NOT_FOUND") {
    // This device remembers a plan the server no longer has.
    resetPlanState();
    const g = activeGoal();
    if (g) g.planId = null;
    saveState();
    setStage("intake");
    $("planEmpty").hidden = false;
    $("planContent").hidden = true;
    renderCalendar();
    syncToolbar();
    toast(d.text, { ref: d.ref });
    return;
  }
  if (d.code === "UNAUTHORIZED") {
    clearToken();
    toast(d.text, { ref: d.ref });
    return;
  }
  // TODO_NOT_FOUND / EVENT_NOT_FOUND / SCHEDULE_CONFLICT all mean local state
  // has gone stale. Repair what we can, then report.
  if (["TODO_NOT_FOUND", "EVENT_NOT_FOUND", "SCHEDULE_CONFLICT"].includes(d.code) && state.planId) {
    try {
      state.plan = await getPlan(state.planId);
      await loadCalendar();
      renderPanel({ animate: false });
    } catch (_) { /* reported just below */ }
  }
  toast(d.text, { ref: d.ref });
}

// ── Plan and calendar ───────────────────────────────────────────────────
async function loadPlan(planId) {
  state.planId = planId;
  state.plan = await getPlan(planId);
  await loadCalendar();
  resetAnchor();
  const g = activeGoal();
  if (g) g.planId = planId;
  saveState();
  setStage("plan");
  renderPanel();
  renderChatList();
}

async function loadCalendar() {
  if (!state.planId) return;
  // Go encodes an empty slice as null, so this is not a redundant guard.
  const list = await getCalendar(state.planId);
  state.events = Array.isArray(list) ? list.filter(Boolean) : [];
}

function renderPanel({ animate = true } = {}) {
  if (!state.plan) return;
  setAnimateRows(animate);
  renderGoalBand(state.plan);
  renderPlan(state.plan);
  renderKit(state.plan);
  renderCalendar();
  syncToolbar();
  renderScheduleNote();
}

// Once it is scheduled, "Schedule it" stops being the primary thing to do.
function syncToolbar() {
  const scheduled = state.events.length > 0;
  const pending = proposedCount();

  const sched = $("scheduleBtn");
  sched.textContent = t(scheduled ? "btn_reschedule" : "btn_schedule");
  sched.classList.toggle("primary", !scheduled);
  sched.disabled = !state.planId;

  $("emptyScheduleBtn").disabled = !state.planId;

  const confirm = $("confirmBtn");
  // Confirming is only meaningful while there is a proposal to promote — and
  // it is never done automatically, because the preview is the point.
  confirm.disabled = pending === 0;
  confirm.classList.toggle("primary", pending > 0);

  $("icsBtn").hidden = !scheduled;
  $("rolloverBtn").disabled = !scheduled;
}

function renderScheduleNote() {
  const note = $("rollMsg");
  const pending = proposedCount();
  const parts = [];
  let cls = "roll-note";

  if (pending > 0) {
    parts.push(t("preview_note", { n: pending }));
    cls += " preview";
  } else if (state.events.length) {
    const confirmed = state.events.filter((e) => e.status === "scheduled" || e.status === "done").length;
    if (confirmed) {
      parts.push(t("confirmed_note", { n: confirmed }));
      cls += " ok";
    }
  }
  // The server's own words about a timeline that does not fit.
  const r = state.lastSchedule;
  if (r && r.deadlineNote) parts.push(String(r.deadlineNote));

  note.className = cls;
  note.textContent = parts.join(" ");
  note.hidden = !parts.length;
}

async function doSchedule() {
  if (!state.planId) return;
  try {
    const r = await apiSchedule(state.planId);
    state.lastSchedule = r;
    state.events = Array.isArray(r.events) ? r.events.filter(Boolean) : [];
    // startDate, finishDate, droppedSessions and the deadline verdict all
    // live on the plan after scheduling, so take the server's copy of it.
    state.plan = await getPlan(state.planId);
    resetAnchor();
    renderPanel({ animate: false });
    updateRail(state.plan, !!r.missesDeadline);
    switchTab("cal");
    toast(t("toast_scheduled", { n: state.events.length, d: state.plan.finishDate || "" }), { kind: "ok" });
    announce(t("toast_scheduled", { n: state.events.length, d: state.plan.finishDate || "" }));
  } catch (e) {
    await handlePanelError(e);
  }
}

async function doConfirm() {
  if (!state.planId) return;
  try {
    const r = await confirmSchedule(state.planId);
    state.lastSchedule = null;
    await loadCalendar();
    renderPanel({ animate: false });
    toast(t("toast_confirmed", { n: r.confirmed || 0, t: r.total || 0 }), { kind: "ok" });
  } catch (e) {
    await handlePanelError(e);
  }
}

// Completes exactly ONE session of a series. `eventId` names which; without
// it the backend takes the soonest outstanding one. There is no un-complete,
// because the API does not offer one.
async function complete(todoId, eventId) {
  if (!state.planId || !todoId) return;
  const keep = $("paneScroll").scrollTop;
  try {
    const r = await completeTodo({ planId: state.planId, todoId, eventId });
    state.plan = await getPlan(state.planId);
    await loadCalendar();
    renderPanel({ animate: false });
    $("paneScroll").scrollTop = keep;
    // The server's own counts, not an assumption about them.
    if (Number.isFinite(r.completedCount) && Number.isFinite(r.plannedCount)) {
      toast(t("toast_session_done", { d: r.completedCount, n: r.plannedCount, r: r.remaining }), { kind: "ok" });
    } else {
      toast(t("toast_logged"), { kind: "ok" });
    }
  } catch (e) {
    await handlePanelError(e);
    $("paneScroll").scrollTop = keep;
  }
}

async function doIcs() {
  if (!state.planId) return;
  try {
    await downloadIcs(state.planId, state.plan && state.plan.skill);
    toast(t("toast_ics"), { kind: "ok" });
  } catch (e) {
    const d = describe(e);
    toast(d.text || t("ics_failed"), { ref: d.ref });
  }
}

// Nightly rollover is a server-side job. This exists only behind ?dev=1 so
// the "moved N×" badge and a shifted finish date can be seen on demand; it is
// not a product control.
async function doRollover() {
  if (!state.planId) return;
  try {
    const r = await apiRollover();
    const mine = (r.results || []).find((x) => x.planId === state.planId) || (r.results || [])[0];
    state.plan = await getPlan(state.planId);
    await loadCalendar();
    renderPanel({ animate: false });
    if (!mine || !mine.moved) {
      toast(t("nothing_roll"), { kind: "ok" });
      return;
    }
    updateRail(state.plan, true);
    toast(t("finish_moved", { old: mine.oldFinish || "", new: mine.newFinish || "", d: mine.finishShiftDays || 0 }));
  } catch (e) {
    await handlePanelError(e);
  }
}

// ── Meter ───────────────────────────────────────────────────────────────
// `enabled: false` means the backend is answering from its mock brain. That
// is worth a badge: a canned plan demoed as a live one is a real problem.
async function checkMeter() {
  const dot = $("meterDot");
  const text = $("meterText");
  if (text) text.textContent = t("meter_connecting");
  try {
    const m = await apiMeter();
    state.meter = m;
    state.mockMode = m && m.enabled === false;
    $("mockBadge").hidden = !state.mockMode;
    $("mockBadge").title = t("demo_badge_title");
    syncAccount();
    if (!DEV) return;
    if (dot) dot.className = "meter-dot " + (state.mockMode ? "mock" : "live");
    if (text) {
      text.textContent = state.mockMode
        ? t("meter_mock")
        : t("meter_live", { n: m.callsTotal || 0, c: Number(m.costUsd || 0).toFixed(3) });
    }
  } catch (_) {
    // The meter is instrumentation. Its absence must never block the app.
    if (text) text.textContent = "";
  }
}

// ── Keyboard ────────────────────────────────────────────────────────────
function wireKeyboard() {
  document.addEventListener("keydown", (e) => {
    if (!(e.ctrlKey || e.metaKey)) return;
    const k = e.key.toLowerCase();
    if (k === "k") {
      e.preventDefault();
      if (!state.sidebarOpen) setSidebar(true);
      $("sbSearch").focus();
      $("sbSearch").select();
    } else if (k === "b") {
      e.preventDefault();
      setSidebar(!state.sidebarOpen);
    } else if (k === "o" && e.shiftKey) {
      e.preventDefault();
      newGoal();
    }
  });

  // Escape steps back out of whatever is layered on top.
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    if (settingsOpen()) {
      if (confirmPending()) hideConfirm();
      else closeSettings();
      return;
    }
    if (acctMenuOpen()) { closeAcctMenu(); $("acctBtn").focus(); return; }
    if (chatMenuOpen()) { closeChatMenu(); return; }
    if (window.innerWidth <= 940 && state.sidebarOpen) { setSidebar(false); $("sbOpen").focus(); return; }
    if (document.body.dataset.stage !== "plan" || !dockIsOpen()) return;
    setDock(false);
    $("dockToggle").focus();
  });
}

// ── The on-screen keyboard ──────────────────────────────────────────────
// On small screens the composer is fixed to the bottom of the viewport. A
// keyboard shrinks the *visual* viewport but not the layout viewport a fixed
// element is pinned to, so without this the composer ends up behind the keys.
// Publishing the overlap as --kb lets the CSS lift the dock by exactly that.
function wireOnScreenKeyboard() {
  const vv = window.visualViewport;
  if (!vv) return;
  let kb = 0;
  const sync = () => {
    const overlap = Math.max(0, window.innerHeight - vv.height - vv.offsetTop);
    // A collapsing browser toolbar also shrinks the visual viewport; only a
    // gap this large is a keyboard.
    const next = overlap > 110 ? Math.round(overlap) : 0;
    if (next === kb) return;
    kb = next;
    document.documentElement.style.setProperty("--kb", kb + "px");
    document.body.classList.toggle("kb-up", kb > 0);
    // The dock just moved; keep the newest message against it.
    if (kb) requestAnimationFrame(scrollToEnd);
  };
  vv.addEventListener("resize", sync);
  vv.addEventListener("scroll", sync);
  sync();
}

boot();
