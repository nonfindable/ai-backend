// ═══ Calendar export ═════════════════════════════════════════════════════
// GET /api/plan/{id}/ics needs the Authorization header, so a plain <a href>
// gets a 401 and downloads an error envelope named like a calendar. Fetch it
// as a blob, drive a temporary anchor, then release the object URL.

import { icsBlob } from "./api.js";

export async function downloadIcs(planId, skill) {
  const blob = await icsBlob(planId);
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename(skill);
  a.rel = "noopener";
  // Firefox requires the anchor to be in the document for a programmatic click.
  a.style.display = "none";
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoked after the download has been handed off, not before.
  setTimeout(() => URL.revokeObjectURL(url), 2000);
}

// The skill is arbitrary user text, and this string becomes a filename.
function filename(skill) {
  const slug = String(skill || "plan")
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48) || "plan";
  return `start-ai-${slug}.ics`;
}
