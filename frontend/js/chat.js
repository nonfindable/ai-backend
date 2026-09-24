// ═══ The conversation ════════════════════════════════════════════════════
// Rendering switches on `turn.stage`, never on the words in `turn.assistant`.
// The prose is for the human; the stage is for this file.
//
// Two rules this module exists to keep:
//   · confirm_plan gets a CARD with real buttons. No plan exists until the
//     learner approves it there, so rendering it as an ordinary bubble stalls
//     the conversation forever waiting for a reply nobody knows to send.
//   · plan_ready is not the end. The composer is never disabled on success,
//     because every question and every change request afterwards goes to the
//     same endpoint.

import { $, el, clear, button, paragraphs, refChip, announce, prefersReducedMotion } from "./dom.js";
import { t } from "./i18n.js";
import { state, hasUserMessage } from "./state.js";

let handlers = {};
let typingNode = null;
let typingTimer = 0;

export function initChat(h) {
  handlers = h || {};
  const form = $("composerForm");
  const input = $("composerInput");

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    submit();
  });
  input.addEventListener("keydown", (e) => {
    // Enter sends; Shift+Enter is a newline. IME composition must not send.
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  input.addEventListener("input", () => {
    autoGrow(input);
    syncComposer();
  });

  $("dockToggle").addEventListener("click", () => setDock(!dockIsOpen()));

  autoGrow(input);
  syncComposer();
}

function submit() {
  const input = $("composerInput");
  const text = input.value.trim();
  if (!text || state.busy || state.quotaBlocked) return;
  input.value = "";
  autoGrow(input);
  syncComposer();
  handlers.onSend && handlers.onSend(text);
}

function autoGrow(input) {
  input.style.height = "auto";
  input.style.height = Math.min(input.scrollHeight, 150) + "px";
}

export function focusComposer() {
  const input = $("composerInput");
  if (input && !input.disabled) input.focus();
}

// ── The dock ──
// Once a plan exists the conversation collapses to a handle above the
// composer. Opening it hands it the working area; the goal band stays, so you
// never lose which plan this is.
export function dockIsOpen() {
  return document.body.dataset.stage !== "plan" || !document.body.classList.contains("dock-closed");
}
export function setDock(open) {
  state.dockOpen = !!open;
  document.body.classList.toggle("dock-closed", !open);
  const btn = $("dockToggle");
  if (btn) btn.setAttribute("aria-expanded", String(!!open));
  syncDock();
  if (open) {
    scrollToEnd();
    focusComposer();
  }
}
export function syncDock() {
  const label = $("dockLabel");
  if (label) label.textContent = t(dockIsOpen() ? "dock_plan" : "dock_label");
}

export function syncComposer() {
  const input = $("composerInput");
  const send = $("sendBtn");
  if (!input || !send) return;
  input.disabled = state.busy || state.quotaBlocked;
  send.disabled = input.disabled || !input.value.trim();
  // The composer asks for a goal during intake, and about the plan afterwards.
  input.placeholder = state.quotaBlocked
    ? t("composer_blocked")
    : t(document.body.dataset.stage === "plan" ? "composer_plan" : "composer");
}

// ── Rendering ──
export function renderConversation({ scroll = true } = {}) {
  const log = $("chatLog");
  if (!log) return;
  const atBottom = isNearBottom(log);
  clear(log);

  state.messages.forEach((m) => log.appendChild(messageNode(m)));
  if (typingNode) log.appendChild(typingNode);

  renderChips();
  renderTurnState();
  syncComposer();
  syncDock();
  if (scroll || atBottom) scrollToEnd();
}

function messageNode(m) {
  if (m.role === "notice") return el("div", "notice", m.text);

  if (m.role === "user") {
    const wrap = el("div", "turn-wrap");
    const turn = el("div", "turn user");
    const bubble = el("div", "bubble");
    bubble.appendChild(paragraphs(m.text));
    turn.appendChild(bubble);
    wrap.appendChild(turn);
    if (m.failed) wrap.appendChild(failureFooter(m));
    return wrap;
  }

  // assistant
  if (m.stage === "confirm_plan") {
    const turn = el("div", "turn ai recap-turn");
    turn.appendChild(avatar());
    turn.appendChild(recapCard(m));
    return turn;
  }

  const turn = el("div", `turn ai${m.stage === "out_of_scope" ? " redirect" : ""}`);
  turn.appendChild(avatar());
  const bubble = el("div", "bubble");
  if (m.tags && m.tags.length) bubble.appendChild(tagStrip(m.tags));
  bubble.appendChild(paragraphs(m.text));
  // Only the turn that actually announces a plan offers a way into it. Every
  // later answer is also stage plan_ready — a link on all of them would be a
  // permanent fixture that says nothing.
  if (m.planId && m.tags && m.tags.includes("plan_ready")) {
    bubble.appendChild(el("div", {
      class: "bubble-actions",
      children: [button(t("view_plan"), { cls: "link", onClick: () => handlers.onViewPlan && handlers.onViewPlan() })],
    }));
  }
  turn.appendChild(bubble);
  return turn;
}

function avatar() {
  return el("div", { class: "avatar", text: "s.", attrs: { "aria-hidden": "true" } });
}

function tagStrip(tags) {
  const strip = el("div", "tags");
  tags.forEach((tag) => {
    const label = {
      plan_ready: t("plan_ready_tag"),
      plan_updated: t("plan_updated_tag"),
      schedule_updated: t("schedule_updated_tag"),
    }[tag];
    if (label) strip.appendChild(el("span", `tag tag-${tag}`, label));
  });
  return strip;
}

// ── Quick replies ──
// One tray under the composer, the shape people already know. They belong to
// the newest turn only: an old chip is an offer the conversation has moved past.
function renderChips() {
  const host = clear($("chips"));
  if (state.busy || state.quotaBlocked) return;
  // The recap gate renders its own choices as buttons inside the card.
  if (state.stage === "confirm_plan") return;

  const opts = state.options && state.options.length
    ? state.options
    : (!hasUserMessage() ? t("starters") : []);
  if (!Array.isArray(opts)) return;

  opts.slice(0, 12).forEach((label, i) => {
    const b = el("button", { class: "chip", text: label, attrs: { type: "button" } });
    if (!prefersReducedMotion()) b.style.animationDelay = Math.min(i * 0.04, 0.3) + "s";
    // A chip sends its own label as the next message — that is the contract.
    b.addEventListener("click", () => handlers.onSend && handlers.onSend(label));
    host.appendChild(b);
  });
}

// ── confirm_plan: the recap the learner must approve ──
// `assistant` carries the reply, the dash-prefixed recap, the verdict and the
// question, joined by blank lines. Split them apart so the recap reads as a
// recap and the ways forward read as buttons.
export function parseRecap(assistant, question) {
  const intro = [];
  const rows = [];
  const verdict = [];
  let seenRow = false;

  String(assistant || "").split("\n").forEach((raw) => {
    const line = raw.trim();
    if (!line) return;
    const m = /^[-–—•]\s*(.+)$/.exec(line);
    if (m) {
      seenRow = true;
      const body = m[1].trim();
      const ci = body.indexOf(":");
      if (ci > 0) rows.push({ label: body.slice(0, ci).trim(), value: body.slice(ci + 1).trim() });
      else rows.push({ label: "", value: body });
      return;
    }
    (seenRow ? verdict : intro).push(line);
  });

  // The question is rendered on its own, so drop the copy of it that the
  // assistant text ends with.
  const q = String(question || "").trim();
  if (q) {
    [intro, verdict].forEach((arr) => {
      while (arr.length && arr[arr.length - 1] === q) arr.pop();
    });
  }
  return { intro, rows, verdict };
}

function recapCard(m) {
  const { intro, rows, verdict } = parseRecap(m.text, m.question);
  const card = el("div", "recap");

  const head = el("div", "recap-head");
  head.appendChild(el("span", "recap-title", t("recap_title")));
  head.appendChild(el("span", "recap-hint", t("recap_approve_hint")));
  card.appendChild(head);

  if (intro.length) card.appendChild(paragraphs(intro.join("\n\n"), "recap-intro"));

  if (rows.length) {
    const dl = el("dl", "recap-rows");
    rows.forEach((r) => {
      if (r.label) {
        dl.appendChild(el("dt", "", r.label));
        dl.appendChild(el("dd", "", r.value));
      } else {
        dl.appendChild(el("dd", "recap-note span2", r.value));
      }
    });
    card.appendChild(dl);
  }

  if (verdict.length) card.appendChild(paragraphs(verdict.join("\n\n"), "recap-verdict"));

  const q = String(m.question || "").trim();
  if (q) card.appendChild(el("p", "recap-q", q));

  if (m.options && m.options.length) {
    const acts = el("div", "recap-acts");
    // A card the conversation has moved past keeps its shape but stops
    // offering choices that would now be answered out of turn.
    const live = state.stage === "confirm_plan" && !state.busy
      && state.messages[state.messages.length - 1] === m;
    m.options.forEach((label, i) => {
      const b = el("button", {
        class: `btn ${i === 0 ? "primary" : ""}`.trim(),
        text: label,
        attrs: { type: "button" },
      });
      b.disabled = !live;
      b.addEventListener("click", () => handlers.onSend && handlers.onSend(label));
      acts.appendChild(b);
    });
    card.appendChild(acts);
  }
  return card;
}

// ── A message that never got an answer ──
// The backend recorded it but produced no reply, so the message stays in the
// thread, marked, with a Retry that sends the same text again.
function failureFooter(m) {
  const wrap = el("div", "fail");
  wrap.appendChild(el("span", "fail-flag", t("not_sent")));
  if (m.failed.message) wrap.appendChild(el("span", "fail-msg", m.failed.message));
  if (m.failed.retryable !== false) {
    wrap.appendChild(button(t("btn_retry"), {
      onClick: () => handlers.onRetry && handlers.onRetry(m.id),
    }));
  }
  if (m.failed.ref) wrap.appendChild(refChip(m.failed.ref));
  return wrap;
}

// ── Intake progress ──
// `progress.max` is a CEILING the interview usually stops well short of, and
// `adaptive` is always true. So: a question number and an indeterminate bar.
// Never a percentage — that would promise questions that are not coming.
function renderTurnState() {
  const note = $("trayNote");
  const text = $("trayNoteText");
  const bar = $("trayBar");
  if (!note || !text || !bar) return;

  const p = state.progress;
  const named = { scope_check: "stage_scope", disambiguation: "stage_disambig" }[state.stage];
  const answered = p && Number.isFinite(p.answered) && p.answered >= 0 ? Math.floor(p.answered) : null;

  // Before the interview proper, the named stage says more than a count of
  // zero would. Once a plan exists, progress stops being news.
  if (state.stage === "plan_ready" || state.stage === "confirm_plan"
    || (answered == null && !named) || !state.messages.length) {
    note.hidden = true;
    bar.hidden = true;
    return;
  }
  note.hidden = false;
  if (state.stage === "intake" && answered != null) {
    const n = answered + 1;
    text.textContent = t("intake_question_n", { n });
    bar.hidden = false;
    bar.classList.add("indeterminate");
    bar.removeAttribute("aria-valuenow");
    bar.removeAttribute("aria-valuemax");
    bar.setAttribute("aria-label", t("aria_progress"));
    bar.setAttribute("aria-valuetext", t("intake_question_n", { n }));
    return;
  }
  text.textContent = t(named || "stage_scope");
  bar.hidden = true;
}

// ── Typing, with the clock running ──
// The plan stage can legitimately take most of the server's 55-second budget.
// A row of dots that long reads as hung, so this counts up and says what it
// is waiting for.
export function startTyping({ planStage = false } = {}) {
  stopTyping();
  const turn = el("div", "turn ai");
  turn.appendChild(avatar());
  const bubble = el("div", "bubble typing");

  const dots = el("span", "think-dots");
  dots.appendChild(el("i"));
  dots.appendChild(el("i"));
  dots.appendChild(el("i"));
  const label = el("span", "think-label", planStage ? t("thinking_plan") : t("thinking"));
  const time = el("span", "think-time", t("elapsed", { s: 0 }));
  const bar = el("div", "think-bar");
  const fill = el("i");
  bar.appendChild(fill);

  bubble.appendChild(dots);
  bubble.appendChild(label);
  bubble.appendChild(time);
  bubble.appendChild(bar);
  turn.appendChild(bubble);
  typingNode = turn;

  const started = Date.now();
  typingTimer = setInterval(() => {
    const s = Math.round((Date.now() - started) / 1000);
    time.textContent = t("elapsed", { s });
    // Elapsed time against the server's budget — not a claim about how much
    // of the work is done.
    fill.style.width = Math.min(98, (s / 55) * 100).toFixed(1) + "%";
    if (s >= 20) label.textContent = t("thinking_long");
    else if (s >= 6 && planStage) label.textContent = t("thinking_plan");
  }, 1000);

  const log = $("chatLog");
  if (log) {
    log.appendChild(turn);
    scrollToEnd();
  }
  announce(planStage ? t("thinking_plan") : t("thinking"));
}

export function stopTyping() {
  clearInterval(typingTimer);
  typingTimer = 0;
  if (typingNode) typingNode.remove();
  typingNode = null;
}

// ── Scrolling ──
function isNearBottom(log) {
  return log.scrollHeight - log.scrollTop - log.clientHeight < 90;
}
export function scrollToEnd() {
  const log = $("chatLog");
  if (log) log.scrollTop = log.scrollHeight;
}
