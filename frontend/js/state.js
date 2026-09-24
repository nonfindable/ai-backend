// ═══ Client state ════════════════════════════════════════════════════════
// The server owns the truth. What lives here is the identity this device was
// issued, the list of conversations it has started, and the transcripts —
// because there is no endpoint that replays a conversation, so a reload would
// otherwise show an empty thread beside a finished plan.
//
// Plans, calendars and completion counts are never trusted from storage: they
// are refetched from /api/plan and /api/calendar every time.

const KEY = "startai.state.v1";
const SIDEBAR_KEY = "startai.sidebar";
const MAX_GOALS = 50;
const MAX_MESSAGES = 160;

export const state = {
  // identity
  userId: null,
  timezone: null,          // the user's zone, as the backend resolved it
  name: "",                // display name, this device only

  // active conversation
  sessionId: null,
  stage: "scope_check",
  messages: [],            // [{id, role, text, stage, question, options, planId, tags, failed}]
  options: [],             // quick replies from the latest turn
  progress: null,          // {answered, max, adaptive} during intake only
  busy: false,
  quotaBlocked: false,     // DAILY_QUOTA_EXCEEDED: composer stays disabled

  // plan + schedule
  planId: null,
  plan: null,
  events: [],
  lastSchedule: null,      // the most recent POST /api/schedule response

  // views
  tab: "plan",             // plan | cal | kit
  calView: "week",         // week | month
  anchor: null,            // a floating YYYY-MM-DD the calendar is centred on
  dockOpen: true,          // the conversation, once a plan exists
  sidebarOpen: true,
  search: "",

  // shell
  mockMode: null,          // true once /api/meter reports enabled:false
  meter: null,

  // conversations this device has started
  goals: [],               // [{id, title, planId, stage, lang, createdAt, updatedAt, messages, …}]
};

let saveTimer = 0;

export function loadState() {
  try {
    state.sidebarOpen = localStorage.getItem(SIDEBAR_KEY) !== "0" && window.innerWidth > 940;
  } catch (_) {
    state.sidebarOpen = window.innerWidth > 940;
  }

  let raw = null;
  try { raw = JSON.parse(localStorage.getItem(KEY) || "null"); }
  catch (_) { return; }   // unreadable or half-written: start clean
  if (!raw || typeof raw !== "object") return;

  state.userId = str(raw.userId);
  state.timezone = str(raw.timezone);
  state.name = typeof raw.name === "string" ? raw.name.slice(0, 40) : "";
  state.goals = Array.isArray(raw.goals)
    ? raw.goals.map(sanitizeGoal).filter(Boolean).slice(0, MAX_GOALS)
    : [];

  const active = str(raw.activeSessionId);
  if (active && state.goals.some((g) => g.id === active)) adopt(active);
}

function str(v) {
  return typeof v === "string" && v ? v : null;
}

// Anything stored by an older build, hand-edited, or half-written by a crash
// has to be survivable. Unknown shapes are dropped, never trusted.
function sanitizeGoal(g) {
  if (!g || typeof g !== "object" || typeof g.id !== "string" || !g.id) return null;
  const now = Date.now();
  const msgs = Array.isArray(g.messages) ? g.messages : [];
  return {
    id: g.id,
    title: typeof g.title === "string" ? g.title.slice(0, 160) : "",
    planId: str(g.planId),
    stage: typeof g.stage === "string" ? g.stage : "scope_check",
    lang: typeof g.lang === "string" ? g.lang : "en",
    createdAt: Number.isFinite(g.createdAt) ? g.createdAt : now,
    updatedAt: Number.isFinite(g.updatedAt) ? g.updatedAt : now,
    options: Array.isArray(g.options) ? g.options.filter((s) => typeof s === "string").slice(0, 12) : [],
    progress: g.progress && typeof g.progress === "object" ? g.progress : null,
    messages: msgs
      .filter((m) => m && typeof m === "object" && typeof m.text === "string"
        && (m.role === "user" || m.role === "assistant" || m.role === "notice"))
      .slice(-MAX_MESSAGES)
      .map((m) => ({
        id: typeof m.id === "string" ? m.id : uid("m"),
        role: m.role,
        text: m.text,
        stage: typeof m.stage === "string" ? m.stage : "",
        question: typeof m.question === "string" ? m.question : "",
        options: Array.isArray(m.options) ? m.options.filter((s) => typeof s === "string") : [],
        planId: str(m.planId),
        tags: Array.isArray(m.tags) ? m.tags.filter((s) => typeof s === "string") : [],
        // A message that never got an answer is restored as retryable, not as
        // a message that succeeded.
        failed: m.failed && typeof m.failed === "object"
          ? {
            code: str(m.failed.code) || "",
            message: typeof m.failed.message === "string" ? m.failed.message : "",
            ref: str(m.failed.ref),
            retryable: m.failed.retryable !== false,
          }
          : null,
      })),
  };
}

export function saveState() {
  // Coalesced: a single turn touches state several times.
  clearTimeout(saveTimer);
  saveTimer = setTimeout(writeState, 120);
}

export function writeState() {
  syncActiveGoal();
  const payload = {
    userId: state.userId,
    timezone: state.timezone,
    name: state.name,
    activeSessionId: state.sessionId,
    goals: state.goals.slice(0, MAX_GOALS),
  };
  if (write(payload)) return;
  // Out of room: shed the oldest transcripts before shedding whole
  // conversations, rather than losing everything.
  payload.goals = payload.goals.map((g, i) => (i > 2 ? { ...g, messages: g.messages.slice(-10) } : g));
  if (write(payload)) return;
  const byAge = payload.goals.slice().sort((a, b) => a.updatedAt - b.updatedAt);
  while (byAge.length > 1) {
    const victim = byAge.shift();
    if (victim.id === state.sessionId) continue;
    payload.goals = payload.goals.filter((g) => g.id !== victim.id);
    if (write(payload)) return;
  }
}

function write(payload) {
  try {
    localStorage.setItem(KEY, JSON.stringify(payload));
    return true;
  } catch (_) {
    return false;   // quota or a locked-down browser; reported by the caller
  }
}

export function saveSidebar(open) {
  state.sidebarOpen = !!open;
  try { localStorage.setItem(SIDEBAR_KEY, open ? "1" : "0"); } catch (_) { /* best effort */ }
}

export function uid(prefix) {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

// ── Goals (conversations) ──
export function startGoal({ sessionId, lang }) {
  state.sessionId = sessionId;
  state.stage = "scope_check";
  state.messages = [];
  state.options = [];
  state.progress = null;
  resetPlanState();
  state.goals.unshift({
    id: sessionId,
    title: "",
    planId: null,
    stage: "scope_check",
    lang,
    createdAt: Date.now(),
    updatedAt: Date.now(),
    options: [],
    progress: null,
    messages: [],
  });
  state.goals = state.goals.slice(0, MAX_GOALS);
  saveState();
}

export function resetPlanState() {
  state.planId = null;
  state.plan = null;
  state.events = [];
  state.lastSchedule = null;
  state.anchor = null;
  state.tab = "plan";
}

export function activeGoal() {
  return state.goals.find((g) => g.id === state.sessionId) || null;
}

// The goal's name is the learner's own first in-scope message — the only
// thing that describes it before a plan exists to supply a skill name.
export function titleGoal(text) {
  const g = activeGoal();
  if (!g || g.title) return;
  g.title = String(text || "").trim().replace(/\s+/g, " ").slice(0, 80);
  saveState();
}

export function renameGoal(id, title) {
  const g = state.goals.find((x) => x.id === id);
  if (!g) return false;
  const clean = String(title || "").trim().replace(/\s+/g, " ").slice(0, 80);
  if (!clean) return false;
  g.title = clean;
  g.updatedAt = Date.now();
  saveState();
  return true;
}

// Returns what was removed and where, so the action can be undone.
export function deleteGoal(id) {
  const index = state.goals.findIndex((g) => g.id === id);
  if (index < 0) return null;
  const [removed] = state.goals.splice(index, 1);
  if (state.sessionId === id) {
    state.sessionId = null;
    state.messages = [];
    state.options = [];
    state.progress = null;
    state.stage = "scope_check";
    resetPlanState();
  }
  saveState();
  return { record: removed, index };
}

export function restoreGoal(removed) {
  if (!removed || !removed.record) return null;
  state.goals.splice(Math.min(removed.index, state.goals.length), 0, removed.record);
  saveState();
  return removed.record.id;
}

export function clearGoals() {
  state.goals = [];
  state.sessionId = null;
  state.messages = [];
  state.options = [];
  state.progress = null;
  state.stage = "scope_check";
  resetPlanState();
  saveState();
}

function syncActiveGoal() {
  const g = activeGoal();
  if (!g) return;
  g.messages = state.messages.slice(-MAX_MESSAGES);
  g.stage = state.stage;
  g.planId = state.planId;
  g.options = state.options.slice(0, 12);
  g.progress = state.progress;
  g.updatedAt = Date.now();
  // Once a plan exists its skill is a better name than the raw first message.
  if (state.plan && state.plan.skill) g.title = String(state.plan.skill).slice(0, 80);
}

function adopt(sessionId) {
  const g = state.goals.find((x) => x.id === sessionId);
  if (!g) return false;
  state.sessionId = g.id;
  state.messages = g.messages.slice();
  state.stage = g.stage || "scope_check";
  state.options = Array.isArray(g.options) ? g.options.slice() : [];
  state.progress = g.progress || null;
  resetPlanState();
  state.planId = g.planId || null;   // the plan itself is refetched, never restored
  return true;
}

export function switchGoal(sessionId) {
  if (sessionId === state.sessionId) return false;
  writeState();                      // flush the conversation we are leaving
  if (!adopt(sessionId)) return false;
  saveState();
  return true;
}

// ── Messages ──
export function pushMessage(msg) {
  const m = {
    id: uid("m"),
    role: "assistant",
    text: "",
    stage: "",
    question: "",
    options: [],
    planId: null,
    tags: [],
    failed: null,
    ...msg,
  };
  state.messages.push(m);
  if (state.messages.length > MAX_MESSAGES) state.messages = state.messages.slice(-MAX_MESSAGES);
  saveState();
  return m;
}
export function findMessage(id) {
  return state.messages.find((m) => m.id === id) || null;
}
export function hasUserMessage() {
  return state.messages.some((m) => m.role === "user");
}

// ── Derived ──
export function planTimezone() {
  return (state.plan && state.plan.timezone) || state.timezone || "UTC";
}
// Sessions of one todo, soonest first, so "log a session" always has a target.
export function eventsForTodo(todoId) {
  return state.events
    .filter((e) => e && e.todoId === todoId)
    .slice()
    .sort((a, b) => String(a.date).localeCompare(String(b.date))
      || String(a.startTime).localeCompare(String(b.startTime)));
}
export function nextOpenEvent(todoId) {
  return eventsForTodo(todoId).find((e) => e.status !== "done" && e.status !== "skipped") || null;
}
export function proposedCount() {
  return state.events.filter((e) => e && e.status === "proposed").length;
}
export function doneCount() {
  return state.events.filter((e) => e && e.status === "done").length;
}
