export const shortSha = (s: string): string => s.slice(0, 8);

const plural = (n: number, unit: string): string => `${n} ${unit}${n === 1 ? '' : 's'} ago`;

/** "3 minutes ago". Falls back to the date for anything older than a month. */
export function timeAgo(iso: string, now: number = Date.now()): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const s = Math.max(0, (now - t) / 1000);
  if (s < 45) return 'just now';
  const m = Math.round(s / 60);
  if (m < 60) return plural(Math.max(1, m), 'minute');
  const h = Math.round(s / 3600);
  if (h < 24) return plural(h, 'hour');
  const d = Math.round(s / 86400);
  if (d < 30) return plural(d, 'day');
  return new Date(t).toLocaleDateString();
}

export function duration(startIso: string, endIso?: string): string {
  if (!endIso) return '';
  const ms = Date.parse(endIso) - Date.parse(startIso);
  if (!(ms >= 0)) return '';
  const s = Math.round(ms / 1000);
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

/** Renders a finding's value for a table cell. */
export function showValue(v: unknown, max = 60): string {
  if (v === undefined) return '—';
  const s = typeof v === 'string' ? JSON.stringify(v) : JSON.stringify(v) ?? String(v);
  return s.length > max ? s.slice(0, max) + '…' : s;
}
