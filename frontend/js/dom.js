// ═══ DOM helpers, theme, toasts ══════════════════════════════════════════
// Everything user-visible is built with textContent, never innerHTML: the
// assistant's prose, the plan's fields and the error messages all come from
// outside this file, and none of them is markup.

import { t } from "./i18n.js";

export const $ = (id) => document.getElementById(id);
export const qs = (sel, root) => (root || document).querySelector(sel);
export const qsa = (sel, root) => [...(root || document).querySelectorAll(sel)];

// el("div", "cls", "text") — or el("div", {class, text, title, attrs, on, children})
export function el(tag, clsOrOpts, text) {
  const node = document.createElement(tag);
  if (typeof clsOrOpts === "string") {
    if (clsOrOpts) node.className = clsOrOpts;
    if (text != null) node.textContent = text;
    return node;
  }
  const o = clsOrOpts || {};
  if (o.class) node.className = o.class;
  if (o.text != null) node.textContent = o.text;
  if (o.title) node.title = o.title;
  if (o.attrs) for (const [k, v] of Object.entries(o.attrs)) { if (v != null) node.setAttribute(k, String(v)); }
  if (o.on) for (const [k, v] of Object.entries(o.on)) node.addEventListener(k, v);
  if (o.children) for (const c of o.children) { if (c) node.appendChild(c); }
  return node;
}
export function clear(node) {
  while (node && node.firstChild) node.removeChild(node.firstChild);
  return node;
}
export function button(text, { cls = "", onClick, attrs, title } = {}) {
  const b = el("button", {
    class: `btn ${cls}`.trim(),
    text,
    title,
    attrs: { type: "button", ...(attrs || {}) },
  });
  if (onClick) b.addEventListener("click", onClick);
  return b;
}

// ── Icons, all on the same 24-unit stroke grid ──
export function icon(path, size = 16, opts = {}) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", String(opts.weight || 2));
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
  p.setAttribute("d", path);
  svg.appendChild(p);
  return svg;
}
export const ICONS = {
  check: "M20 6 9 17l-5-5",
  // A plus, not a tick: this adds one completed session to a series, it does
  // not mark the whole thing done.
  plus: "M12 5v14M5 12h14",
  flag: "M6 21V4M6 4h11l-2.2 3.5L17 11H6",
  warn: "M12 9v4M12 17h.01M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z",
  dots: "M5.5 12h.01M12 12h.01M18.5 12h.01",
  left: "M15 18l-6-6 6-6",
  right: "M9 6l6 6-6 6",
};
// The row menu's three dots are filled circles, not a stroked path.
export function dotsIcon(size = 15) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("fill", "currentColor");
  svg.setAttribute("aria-hidden", "true");
  [5.5, 12, 18.5].forEach((cx) => {
    const c = document.createElementNS("http://www.w3.org/2000/svg", "circle");
    c.setAttribute("cx", String(cx));
    c.setAttribute("cy", "12");
    c.setAttribute("r", "1.6");
    svg.appendChild(c);
  });
  return svg;
}

// Prose from the backend arrives as paragraphs separated by blank lines. Each
// becomes its own <p>; nothing is parsed for meaning.
export function paragraphs(text, cls) {
  const wrap = el("div", cls || "prose");
  const paras = String(text == null ? "" : text)
    .split(/\n{2,}/)
    .map((s) => s.trim())
    .filter(Boolean);
  paras.forEach((para) => {
    const p = el("p");
    // A single newline inside a paragraph is a line break, not a new block.
    para.split("\n").forEach((line, i) => {
      if (i) p.appendChild(el("br"));
      p.appendChild(document.createTextNode(line));
    });
    wrap.appendChild(p);
  });
  if (!wrap.childNodes.length) wrap.appendChild(el("p", "", String(text || "")));
  return wrap;
}

// ── Theme ──
// The stored preference is light, dark or system; "system" keeps following the
// OS after the choice is made instead of freezing at whatever it was.
const THEME_KEY = "startai.theme";
export function themePref() {
  try { return localStorage.getItem(THEME_KEY) || "system"; } catch (_) { return "system"; }
}
export function applyTheme(pref) {
  try { localStorage.setItem(THEME_KEY, pref); } catch (_) { /* best effort */ }
  const dark = pref === "dark" || (pref === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
  syncThemeButton();
}
export function isDark() {
  return document.documentElement.getAttribute("data-theme") === "dark";
}
// A toggle should say which state it is in, not only look like it.
export function syncThemeButton() {
  const b = $("themeToggle");
  if (!b) return;
  const dark = isDark();
  b.setAttribute("aria-pressed", String(dark));
  b.setAttribute("aria-label", t(dark ? "aria_theme_light" : "aria_theme_dark"));
  b.title = t(dark ? "aria_theme_light" : "aria_theme_dark");
}

// ── Toasts: the one thing allowed to float ──
export function toast(message, { kind = "", ref = null, action = null, timeout = 5600 } = {}) {
  const host = $("toasts");
  if (!host) return null;
  const node = el("div", `toast ${kind}`.trim());
  // An error says so; a confirmation just reports.
  if (kind !== "ok") node.appendChild(el("b", "", t("err_title")));
  node.appendChild(el("span", "toast-msg", message));
  if (action && action.label) {
    const b = el("button", { class: "toast-act", text: action.label, attrs: { type: "button" } });
    b.addEventListener("click", () => {
      node.remove();
      action.onClick && action.onClick();
    });
    node.appendChild(b);
  }
  if (ref) node.appendChild(refChip(ref));
  host.appendChild(node);
  const kill = () => node.remove();
  if (timeout) setTimeout(kill, timeout);
  return { remove: kill, node };
}

// A support code is never the error. It is small, quiet and copyable.
export function refChip(ref) {
  const b = el("button", {
    class: "ref",
    text: t("err_ref", { ref }),
    attrs: { type: "button", title: t("copy") },
  });
  b.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(ref);
      b.textContent = t("copied");
      setTimeout(() => { b.textContent = t("err_ref", { ref }); }, 1400);
    } catch (_) {
      // Clipboard denied: select the text so it can be copied by hand.
      const r = document.createRange();
      r.selectNodeContents(b);
      const sel = getSelection();
      sel.removeAllRanges();
      sel.addRange(r);
    }
  });
  return b;
}

// ── A quiet channel for what a sighted user simply sees happen ──
export function announce(message) {
  const n = $("live");
  if (!n) return;
  n.textContent = "";
  setTimeout(() => { n.textContent = message; }, 60);
}

export function prefersReducedMotion() {
  return matchMedia("(prefers-reduced-motion: reduce)").matches;
}
