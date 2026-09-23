// ═══ Floating civil dates and clock times ═══════════════════════════════
//
// THE RULE THIS FILE EXISTS TO ENFORCE:
//
//   A CalendarEvent's `date` ("2026-10-05") and `startTime` ("18:00") are
//   FLOATING values belonging to plan.timezone. They are not instants, they
//   are not UTC, and they have nothing to do with the browser's zone.
//
//   So: no `new Date(iso)` anywhere near them. Not to get a weekday, not to
//   add a day, not to format. Passing "2026-10-05" to the Date constructor
//   parses it as midnight UTC and then renders it in the viewer's zone, which
//   moves every session in Tashkent back to the 4th for anyone west of UTC.
//
// Every function below that touches a floating value works on integers, with
// no Date object involved at all. The three functions that DO use Date are
// `todayInZone`, `nowInZone` and `formatInstant` — the first two because
// "what day is it in Tashkent?" genuinely needs the clock, the third because
// createdAt / updatedAt / changeLog.at are real RFC3339 instants. All three
// pass `timeZone` to Intl explicitly.

const DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;
const TIME_RE = /^(\d{1,2}):(\d{2})/;

// ── Parsing and formatting the wire shapes ──
export function parseDate(iso) {
  const m = DATE_RE.exec(String(iso || "").trim());
  if (!m) return null;
  const y = +m[1], mo = +m[2], d = +m[3];
  if (mo < 1 || mo > 12 || d < 1 || d > daysInMonth(y, mo)) return null;
  return { y, m: mo, d };
}
export function isDate(iso) {
  return parseDate(iso) != null;
}
export function toISO({ y, m, d }) {
  return `${String(y).padStart(4, "0")}-${String(m).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
}
export function parseTime(hhmm) {
  const m = TIME_RE.exec(String(hhmm || "").trim());
  if (!m) return null;
  const h = +m[1], min = +m[2];
  if (h > 23 || min > 59) return null;
  return { h, min };
}
// Times are shown as they arrived. Zero-padding is the only normalization.
export function formatTime(hhmm) {
  const t = parseTime(hhmm);
  if (!t) return String(hhmm || "");
  return `${String(t.h).padStart(2, "0")}:${String(t.min).padStart(2, "0")}`;
}
// A session's end, by arithmetic on the clock face. Past midnight it wraps and
// says so, rather than silently reporting a smaller number than the start.
export function endTime(hhmm, durationMin) {
  const t = parseTime(hhmm);
  if (!t || !Number.isFinite(durationMin)) return null;
  const total = t.h * 60 + t.min + Math.max(0, Math.round(durationMin));
  const wrapped = ((total % 1440) + 1440) % 1440;
  return {
    time: `${String(Math.floor(wrapped / 60)).padStart(2, "0")}:${String(wrapped % 60).padStart(2, "0")}`,
    nextDay: total >= 1440,
  };
}

// ── Calendar arithmetic, integers only ──
export function isLeap(y) {
  return (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0;
}
export function daysInMonth(y, m) {
  if (m === 2) return isLeap(y) ? 29 : 28;
  return [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][m - 1] || 30;
}

// Days since 1970-01-01, by Howard Hinnant's days_from_civil. Proleptic
// Gregorian, exact for every year we will ever be handed, and — the point —
// entirely free of timezones.
export function dayNumber(iso) {
  const p = typeof iso === "string" ? parseDate(iso) : iso;
  if (!p) return null;
  let y = p.y;
  const m = p.m, d = p.d;
  y -= m <= 2 ? 1 : 0;
  const era = Math.floor(y / 400);
  const yoe = y - era * 400;
  const doy = Math.floor((153 * (m + (m > 2 ? -3 : 9)) + 2) / 5) + d - 1;
  const doe = yoe * 365 + Math.floor(yoe / 4) - Math.floor(yoe / 100) + doy;
  return era * 146097 + doe - 719468;
}
// The inverse: civil_from_days.
export function dateFromDayNumber(n) {
  let z = n + 719468;
  const era = Math.floor(z / 146097);
  const doe = z - era * 146097;
  const yoe = Math.floor((doe - Math.floor(doe / 1460) + Math.floor(doe / 36524) - Math.floor(doe / 146096)) / 365);
  const y = yoe + era * 400;
  const doy = doe - (365 * yoe + Math.floor(yoe / 4) - Math.floor(yoe / 100));
  const mp = Math.floor((5 * doy + 2) / 153);
  const d = doy - Math.floor((153 * mp + 2) / 5) + 1;
  const m = mp + (mp < 10 ? 3 : -9);
  return { y: y + (m <= 2 ? 1 : 0), m, d };
}
export function addDays(iso, n) {
  const dn = dayNumber(iso);
  if (dn == null) return null;
  return toISO(dateFromDayNumber(dn + n));
}
// b − a, in whole days.
export function daysBetween(a, b) {
  const da = dayNumber(a), db = dayNumber(b);
  if (da == null || db == null) return 0;
  return db - da;
}
// 0 = Monday … 6 = Sunday. 1970-01-01 was a Thursday, which is index 3.
export function weekdayIndex(iso) {
  const dn = dayNumber(iso);
  if (dn == null) return null;
  return ((((dn % 7) + 7) % 7) + 3) % 7;
}
export function mondayOf(iso) {
  const wd = weekdayIndex(iso);
  if (wd == null) return null;
  return addDays(iso, -wd);
}
export function weekDates(mondayISO) {
  return Array.from({ length: 7 }, (_, i) => addDays(mondayISO, i));
}
export function firstOfMonth(iso) {
  const p = parseDate(iso);
  return p ? toISO({ y: p.y, m: p.m, d: 1 }) : null;
}
export function addMonths(iso, n) {
  const p = parseDate(iso);
  if (!p) return null;
  const total = p.y * 12 + (p.m - 1) + n;
  const y = Math.floor(total / 12);
  const m = (total % 12) + 1;
  return toISO({ y, m, d: Math.min(p.d, daysInMonth(y, m)) });
}
// The 6×7 block a month view draws: whole weeks, Monday first, the month's
// own days somewhere in the middle.
export function monthGrid(iso) {
  const first = firstOfMonth(iso);
  if (!first) return [];
  const start = mondayOf(first);
  return Array.from({ length: 42 }, (_, i) => addDays(start, i));
}
export function sameMonth(a, b) {
  const pa = parseDate(a), pb = parseDate(b);
  return !!pa && !!pb && pa.y === pb.y && pa.m === pb.m;
}

// ── "Today", in the plan's zone ──
// The only correct way to ask this question: let Intl resolve the wall clock
// in a named zone, then keep the answer as a plain string.
export function todayInZone(tz) {
  return partsInZone(tz).date;
}
export function nowInZone(tz) {
  return partsInZone(tz);
}
function partsInZone(tz) {
  const now = new Date();
  try {
    const parts = new Intl.DateTimeFormat("en-US", {
      timeZone: tz || undefined,
      year: "numeric", month: "2-digit", day: "2-digit",
      hour: "2-digit", minute: "2-digit", hour12: false,
    }).formatToParts(now);
    const get = (k) => (parts.find((p) => p.type === k) || {}).value;
    const y = get("year"), m = get("month"), d = get("day");
    let h = get("hour");
    const min = get("minute");
    if (h === "24") h = "00"; // some engines report midnight as hour 24
    if (y && m && d) {
      return { date: `${y}-${m}-${d}`, time: `${h || "00"}:${min || "00"}` };
    }
  } catch (_) {
    // An unknown or malformed zone name: fall through to the device clock
    // rather than throwing on every render.
  }
  return {
    date: toISO({ y: now.getFullYear(), m: now.getMonth() + 1, d: now.getDate() }),
    time: `${String(now.getHours()).padStart(2, "0")}:${String(now.getMinutes()).padStart(2, "0")}`,
  };
}
export function browserTimezone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch (_) {
    return "UTC";
  }
}

// ── Real instants ──
// createdAt / updatedAt / changeLog.at are RFC3339, so Date is correct here —
// with the zone named explicitly, never left to the device.
export function formatInstant(rfc3339, tz, locale) {
  const dt = new Date(rfc3339);
  if (Number.isNaN(dt.getTime())) return "";
  try {
    return new Intl.DateTimeFormat(locale || "en", {
      timeZone: tz || undefined,
      year: "numeric", month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", hour12: false,
    }).format(dt);
  } catch (_) {
    return dt.toISOString().slice(0, 16).replace("T", " ");
  }
}

// ── Weekday codes ──
// The backend's canonical Mon..Sun codes, in its own order.
export const WEEKDAY_CODES = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
export function weekdayCode(iso) {
  const i = weekdayIndex(iso);
  return i == null ? "" : WEEKDAY_CODES[i];
}
// plan.days is free-form enough to arrive as "Mon" or "Monday"; match on the
// three-letter stem so both work.
export function normalizeDayCode(s) {
  const v = String(s || "").trim().slice(0, 3).toLowerCase();
  return WEEKDAY_CODES.find((c) => c.toLowerCase() === v) || "";
}
