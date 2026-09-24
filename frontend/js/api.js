// ═══ The backend, as one module ══════════════════════════════════════════
// Same origin: the Go server serves this folder, so every path here is
// relative and there is no CORS story to get wrong.
//
// Auth: POST /api/session is the only public route, and it returns the bearer
// token EXACTLY ONCE, at first creation. It cannot be re-fetched — losing it
// orphans every plan the user has — so it is persisted the moment it arrives
// and sent on every other call.

const TOKEN_KEY = "startai.token";

// Every failure the backend produces arrives in one envelope:
//   { "error": { "code", "message", "ref"? } }
// The message is written by the backend to be shown to the user verbatim, so
// unlike most clients this one does not second-guess it. `ref` is an opaque
// support code and is never presented as the error itself.
export class ApiError extends Error {
  constructor(code, message, { ref = null, status = 0, retryable = false } = {}) {
    // `message` is the backend's own user-facing sentence, and nothing else
    // is allowed to stand in for it. Falling back to the code here would put
    // the string "NETWORK" in front of a user instead of a sentence, and it
    // would hide from callers that no message arrived at all.
    super(message || "");
    this.name = "ApiError";
    this.code = code;
    this.ref = ref;
    this.status = status;
    this.retryable = retryable;
  }
}

// Worth offering a Retry button for. The user's message was recorded by the
// backend but no assistant reply was produced, so resending is the right move.
const RETRYABLE = new Set(["AI_UNAVAILABLE", "AI_RATE_LIMITED", "INTERNAL_ERROR", "NETWORK"]);

// A code with no envelope behind it, used when fetch itself fails. The caller
// supplies the copy, because the backend never got a chance to.
export const NETWORK = "NETWORK";

let token = null;
try { token = localStorage.getItem(TOKEN_KEY) || null; } catch (_) { token = null; }

export function hasToken() {
  return !!token;
}
function keepToken(next) {
  token = next || null;
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch (_) { /* private mode: the token then lasts for this tab only */ }
}
export function clearToken() {
  keepToken(null);
}

function authHeaders(extra) {
  const h = { ...(extra || {}) };
  if (token) h.Authorization = "Bearer " + token;
  return h;
}

async function readErrorBody(res) {
  let code = res.status >= 500 ? "INTERNAL_ERROR" : "INVALID_REQUEST";
  let message = "";
  let ref = null;
  try {
    const body = await res.json();
    const err = body && body.error;
    if (err && typeof err === "object") {
      if (typeof err.code === "string" && err.code) code = err.code;
      // Shown verbatim: the backend looks this up from the code and never
      // lets an upstream provider's words reach it.
      if (typeof err.message === "string" && err.message.trim()) message = err.message.trim();
      if (typeof err.ref === "string" && err.ref.trim()) ref = err.ref.trim();
    }
  } catch (_) {
    // A body that isn't the envelope (an HTML error page from a proxy, an
    // empty 502) carries nothing showable. The code stands on its own.
  }
  if (res.status === 401) code = "UNAUTHORIZED";
  return new ApiError(code, message, { ref, status: res.status, retryable: RETRYABLE.has(code) });
}

async function request(path, { method = "GET", body, signal, raw = false } = {}) {
  let res;
  try {
    res = await fetch(path, {
      method,
      signal,
      headers: authHeaders(body === undefined ? undefined : { "Content-Type": "application/json" }),
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) {
    if (e && e.name === "AbortError") throw e;
    throw new ApiError(NETWORK, "", { retryable: true });
  }
  if (!res.ok) throw await readErrorBody(res);
  if (raw) return res;
  try {
    return await res.json();
  } catch (_) {
    throw new ApiError("INTERNAL_ERROR", "", { status: res.status, retryable: true });
  }
}

// ── Session ──
// Called with no token: creates a new user and returns one.
// Called WITH a token: same user, brand-new conversation, and no token in the
// response — which is exactly how a learner starts a second goal.
export async function createSession({ name = "", timezone, lang }) {
  const fresh = !token;
  const r = await request("/api/session", { method: "POST", body: { name, timezone, lang } });
  if (typeof r.token === "string" && r.token) keepToken(r.token);
  return { ...r, isNewUser: fresh };
}

// ── Conversation ──
// One endpoint for the whole relationship: scope check, interview, the recap
// gate, and every question or change request after the plan exists.
export function chat({ sessionId, message, lang, signal }) {
  return request("/api/chat", { method: "POST", body: { sessionId, message, lang }, signal });
}

// ── Plan ──
export function getPlan(planId) {
  return request(`/api/plan/${encodeURIComponent(planId)}`);
}

// ── Scheduling ──
export function schedule(planId) {
  return request("/api/schedule", { method: "POST", body: { planId } });
}
export function confirmSchedule(planId) {
  return request("/api/schedule/confirm", { method: "POST", body: { planId } });
}
export function getCalendar(planId) {
  const q = planId ? `?planId=${encodeURIComponent(planId)}` : "";
  return request(`/api/calendar${q}`);
}
// `eventId` completes that specific session; omitting it completes the soonest
// outstanding one of the series.
export function completeTodo({ planId, todoId, eventId }) {
  const body = { planId, todoId };
  if (eventId) body.eventId = eventId;
  return request("/api/todo/complete", { method: "POST", body });
}
export function rollover(asOf) {
  return request("/api/rollover", { method: "POST", body: asOf ? { asOf } : {} });
}

// ── Meter ──
export function meter() {
  return request("/api/meter");
}

// ── ICS ──
// This route needs the Authorization header, so a plain <a href> gets a 401.
// Fetch it as a blob and drive a temporary anchor instead.
export async function icsBlob(planId) {
  const res = await request(`/api/plan/${encodeURIComponent(planId)}/ics`, { raw: true });
  return res.blob();
}
