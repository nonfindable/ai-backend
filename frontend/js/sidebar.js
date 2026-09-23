// ═══ History, account and settings ═══════════════════════════════════════
// The chat history down the left, the account row at its foot, and the
// settings sheet those two open. Everything here is this device's own state:
// no plan, no calendar and no completion count is ever read from storage.

import { $, el, clear, dotsIcon, toast, applyTheme, themePref, syncThemeButton } from "./dom.js";
import { t, lang, LANGS } from "./i18n.js";
import {
  state, saveState, writeState, saveSidebar, renameGoal, deleteGoal, restoreGoal, clearGoals,
} from "./state.js";
import { planTimezone } from "./state.js";
import { browserTimezone } from "./dates.js";

let handlers = {};
let menuFor = null;      // which chat row the dots menu belongs to
let acctOpen = false;
let confirmAction = null;
let nameTimer = 0;

export function initSidebar(h) {
  handlers = h || {};

  $("newChatBtn").addEventListener("click", () => handlers.onNewChat && handlers.onNewChat());
  $("sbCollapse").addEventListener("click", () => setSidebar(false));
  $("sbOpen").addEventListener("click", () => setSidebar(true));
  $("sbScrim").addEventListener("click", () => setSidebar(false));

  $("sbSearch").addEventListener("input", (e) => {
    state.search = e.target.value;
    renderChatList();
  });

  // The row menu is placed once, in viewport coordinates. Scrolling the list
  // under it would leave it pointing at a different chat, so it closes.
  $("sbList").addEventListener("scroll", () => { if (menuFor) closeChatMenu(); }, { passive: true });

  $("sbMenu").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-act]");
    if (!b || !menuFor) return;
    const id = menuFor;
    closeChatMenu();
    if (b.dataset.act === "rename") startRename(id);
    else if (b.dataset.act === "delete") removeChat(id);
  });

  $("acctBtn").addEventListener("click", () => setAcctMenu(!acctOpen));
  $("acctMenu").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-act]");
    if (!b) return;
    setAcctMenu(false);
    const act = b.dataset.act;
    if (act === "profile") openSettings("set-profile");
    else if (act === "appearance") openSettings("set-appearance");
    else if (act === "shortcuts") openSettings("set-shortcuts");
    else if (act === "settings") openSettings();
    else if (act === "reset") { openSettings("set-data"); askReset(); }
  });

  document.addEventListener("click", (e) => {
    if (menuFor && !e.target.closest("#sbMenu") && !e.target.closest(".sb-item-menu")) closeChatMenu();
    if (acctOpen && !e.target.closest("#acctMenu") && !e.target.closest("#acctBtn")) setAcctMenu(false);
  });

  // ── Settings ──
  $("setClose").addEventListener("click", closeSettings);
  $("settings").addEventListener("click", (e) => { if (e.target.id === "settings") closeSettings(); });

  $("setName").addEventListener("input", (e) => {
    const v = e.target.value.slice(0, 40);
    state.name = v;
    clearTimeout(nameTimer);
    nameTimer = setTimeout(() => {
      saveState();
      syncAccount();
      handlers.onNameChange && handlers.onNameChange(v);
    }, 300);
  });

  $("themePick").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-theme-pref]");
    if (!b) return;
    applyTheme(b.dataset.themePref);
    syncSettings();
  });
  $("langPick").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-lang]");
    if (!b || !LANGS.includes(b.dataset.lang)) return;
    handlers.onLangChange && handlers.onLangChange(b.dataset.lang);
  });

  $("clearChatsBtn").addEventListener("click", askClear);
  $("resetBtn").addEventListener("click", askReset);
  $("setConfirmNo").addEventListener("click", hideConfirm);
  $("setConfirmYes").addEventListener("click", () => {
    const act = confirmAction;
    hideConfirm();
    if (act === "clear") {
      clearGoals();
      renderChatList();
      toast(t("done_clear"), { kind: "ok" });
      handlers.onNewChat && handlers.onNewChat();
    } else if (act === "reset") {
      handlers.onReset && handlers.onReset();
    }
  });
}

// ── Open / closed ──
export function setSidebar(open) {
  saveSidebar(open);
  document.documentElement.setAttribute("data-sb", open ? "open" : "closed");
  document.body.classList.toggle("sb-closed", !open);
  document.body.classList.toggle("sb-open", !!open);
  $("sbCollapse").setAttribute("aria-label", t("aria_sidebar_close"));
  $("sbOpen").setAttribute("aria-label", t("aria_sidebar_open"));
  if (open && window.innerWidth > 940) $("sbSearch").focus();
}
export function sidebarIsOpen() {
  return state.sidebarOpen;
}

// ── The list ──
// Grouped by when you last touched it. These are local bookkeeping stamps,
// so the device's own clock is the right one to read them with.
function groupOf(ms) {
  const now = new Date();
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86400000;
  if (ms >= midnight) return "grp_today";
  if (ms >= midnight - day) return "grp_yesterday";
  if (ms >= midnight - day * 7) return "grp_week";
  if (ms >= midnight - day * 30) return "grp_month";
  return "grp_older";
}
const GROUP_ORDER = ["grp_today", "grp_yesterday", "grp_week", "grp_month", "grp_older"];

export function renderChatList() {
  const host = clear($("sbList"));
  const q = String(state.search || "").trim().toLowerCase();
  const list = state.goals.filter((g) => !q || (g.title || t("chat_untitled")).toLowerCase().includes(q));

  if (!list.length) {
    host.appendChild(el("p", "sb-none", t(state.goals.length ? "chats_no_match" : "chats_none")));
    return;
  }

  const buckets = new Map();
  list.forEach((g) => {
    const key = groupOf(g.updatedAt || g.createdAt);
    if (!buckets.has(key)) buckets.set(key, []);
    buckets.get(key).push(g);
  });

  GROUP_ORDER.forEach((key) => {
    const rows = buckets.get(key);
    if (!rows || !rows.length) return;
    host.appendChild(el("div", "sb-group", t(key)));
    rows
      .slice()
      .sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0))
      .forEach((g) => host.appendChild(chatRow(g)));
  });
}

function chatRow(g) {
  const row = el("div", `sb-item${g.id === state.sessionId ? " active" : ""}`);
  row.dataset.id = g.id;

  const main = el("button", {
    class: "sb-item-main",
    text: g.title || t("chat_untitled"),
    title: g.title || t("chat_untitled"),
    attrs: { type: "button" },
  });
  main.addEventListener("click", () => handlers.onOpenChat && handlers.onOpenChat(g.id));
  row.appendChild(main);

  const dots = el("button", {
    class: "sb-item-menu",
    attrs: { type: "button", "aria-label": t("aria_chat_menu", { title: g.title || t("chat_untitled") }) },
  });
  dots.appendChild(dotsIcon(15));
  dots.addEventListener("click", (e) => {
    e.stopPropagation();
    openChatMenu(g.id, dots);
  });
  row.appendChild(dots);
  return row;
}

function openChatMenu(id, near) {
  const menu = $("sbMenu");
  menuFor = id;
  menu.hidden = false;
  document.querySelectorAll(".sb-item").forEach((n) => n.classList.toggle("menu-open", n.dataset.id === id));
  // Placed in viewport coordinates, then nudged back inside if it would hang
  // off the bottom or the right.
  const r = near.getBoundingClientRect();
  menu.style.left = "0px";
  menu.style.top = "0px";
  const m = menu.getBoundingClientRect();
  const left = Math.min(r.left, window.innerWidth - m.width - 8);
  const top = Math.min(r.bottom + 4, window.innerHeight - m.height - 8);
  menu.style.left = Math.max(8, left) + "px";
  menu.style.top = Math.max(8, top) + "px";
  const first = menu.querySelector("button");
  if (first) first.focus();
}

export function closeChatMenu() {
  menuFor = null;
  $("sbMenu").hidden = true;
  document.querySelectorAll(".sb-item.menu-open").forEach((n) => n.classList.remove("menu-open"));
}
export function chatMenuOpen() {
  return !!menuFor;
}

function startRename(id) {
  const row = document.querySelector(`.sb-item[data-id="${CSS.escape(id)}"]`);
  const g = state.goals.find((x) => x.id === id);
  if (!row || !g) return;
  const main = row.querySelector(".sb-item-main");
  if (!main) return;

  const input = el("input", { class: "sb-rename", attrs: { type: "text", maxlength: "80", value: g.title || "" } });
  row.replaceChild(input, main);
  input.focus();
  input.select();

  let settled = false;
  const finish = (commit) => {
    if (settled) return;
    settled = true;
    if (commit) renameGoal(id, input.value);
    renderChatList();
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); finish(true); }
    else if (e.key === "Escape") { e.preventDefault(); finish(false); }
  });
  input.addEventListener("blur", () => finish(true));
}

// Deleting is immediate and undoable, rather than guarded by a modal nobody
// reads. A browser confirm() would also block the page outright.
function removeChat(id) {
  const g = state.goals.find((x) => x.id === id);
  const title = (g && g.title) || t("chat_untitled");
  const wasActive = id === state.sessionId;
  const removed = deleteGoal(id);
  if (!removed) return;
  renderChatList();
  if (wasActive) handlers.onChatRemoved && handlers.onChatRemoved();

  toast(t("chat_deleted", { title }), {
    kind: "ok",
    timeout: 8000,
    action: {
      label: t("undo"),
      onClick: () => {
        const back = restoreGoal(removed);
        renderChatList();
        if (back && wasActive) handlers.onOpenChat && handlers.onOpenChat(back);
      },
    },
  });
}

// ── Account ──
export function syncAccount() {
  const name = (state.name || "").trim();
  const shown = name || t("acct_guest");
  const initial = (name ? name[0] : "G").toUpperCase();
  // What the app is actually talking to, said where the account lives.
  const plan = state.mockMode ? t("acct_demo") : t("acct_live");

  [["acctName", shown], ["amName", shown], ["acctPlan", plan], ["amPlan", plan],
    ["acctAv", initial], ["amAv", initial]].forEach(([id, text]) => {
    const n = $(id);
    if (n) n.textContent = text;
  });
}

function setAcctMenu(open) {
  acctOpen = !!open;
  const menu = $("acctMenu");
  menu.hidden = !open;
  $("acctBtn").setAttribute("aria-expanded", String(!!open));
  if (!open) return;
  // Opens upward from the account row it belongs to.
  const r = $("acctBtn").getBoundingClientRect();
  menu.style.left = "0px";
  menu.style.top = "0px";
  const m = menu.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(r.left, window.innerWidth - m.width - 8)) + "px";
  menu.style.top = Math.max(8, r.top - m.height - 6) + "px";
  const first = menu.querySelector("button");
  if (first) first.focus();
}
export function acctMenuOpen() {
  return acctOpen;
}
export function closeAcctMenu() {
  setAcctMenu(false);
}

// ── Settings ──
export function openSettings(sectionId) {
  $("settings").hidden = false;
  syncSettings();
  if (sectionId) {
    const sec = $(sectionId);
    if (sec) sec.scrollIntoView({ block: "start", behavior: "auto" });
  }
  const name = $("setName");
  if (name && sectionId === "set-profile") name.focus();
  else $("setClose").focus();
}
export function closeSettings() {
  hideConfirm();
  $("settings").hidden = true;
  $("acctBtn").focus();
}
export function settingsOpen() {
  return !$("settings").hidden;
}

export function syncSettings() {
  if (!settingsOpen()) return;

  $("setName").value = state.name || "";

  const pref = themePref();
  $("themePick").querySelectorAll("button[data-theme-pref]").forEach((b) => {
    b.setAttribute("aria-pressed", String(b.dataset.themePref === pref));
  });
  $("langPick").querySelectorAll("button[data-lang]").forEach((b) => {
    b.setAttribute("aria-pressed", String(b.dataset.lang === lang));
  });

  // Scheduling happens in the plan's timezone, which the backend owns.
  const tz = planTimezone();
  const device = browserTimezone();
  $("setTz").textContent = tz;
  $("setTzNote").textContent = tz === device ? t("tz_same") : t("tz_differs", { device });

  const chats = state.goals.length;
  $("setStorage").textContent = t("set_storage", { c: chats, n: storageSize() });

  renderShortcuts();
}

function storageSize() {
  try {
    const raw = localStorage.getItem("startai.state.v1") || "";
    const kb = raw.length / 1024;
    return kb < 1024 ? `${kb.toFixed(1)} KB` : `${(kb / 1024).toFixed(1)} MB`;
  } catch (_) {
    return "—";
  }
}

const IS_MAC = /mac|iphone|ipad/i.test(navigator.platform || navigator.userAgent || "");
const MOD = IS_MAC ? "⌘" : "Ctrl";

function renderShortcuts() {
  const host = clear($("kbdList"));
  [
    ["sc_new", `${MOD}+Shift+O`],
    ["sc_search", `${MOD}+K`],
    ["sc_sidebar", `${MOD}+B`],
    ["sc_send", "Enter"],
    ["sc_esc", "Esc"],
  ].forEach(([key, keys]) => {
    const li = el("li");
    li.appendChild(el("span", "", t(key)));
    li.appendChild(el("kbd", "", keys));
    host.appendChild(li);
  });
}

function askClear() {
  confirmAction = "clear";
  showConfirm(t("ask_clear"));
}
function askReset() {
  confirmAction = "reset";
  showConfirm(t("ask_reset"));
}
function showConfirm(text) {
  $("setConfirmText").textContent = text;
  $("setConfirm").hidden = false;
  $("setConfirmYes").focus();
}
export function hideConfirm() {
  confirmAction = null;
  $("setConfirm").hidden = true;
}
export function confirmPending() {
  return !!confirmAction;
}

// Called before the page goes away, so nothing in flight is lost.
export function flush() {
  clearTimeout(nameTimer);
  writeState();
}

// Keeps the theme button's label in step after a language change.
export { syncThemeButton };
