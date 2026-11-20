import type { Diff, Severity } from './types';

/** Findings that share an endpoint, kind and path: one problem seen on several requests. */
export interface Group {
  example: Diff; // the first occurrence
  count: number;
  requestIds: number[];
}

const MAX_IDS = 20;

export function groupDiffs(diffs: Diff[], severity?: Severity): Group[] {
  const groups = new Map<string, Group>();
  for (const d of diffs) {
    if (severity && d.severity !== severity) continue;
    const key = `${d.severity}\u0000${d.endpoint}\u0000${d.kind}\u0000${d.json_path}`;
    const g = groups.get(key);
    if (g) {
      g.count++;
      if (g.requestIds.length < MAX_IDS && !g.requestIds.includes(d.request_id)) g.requestIds.push(d.request_id);
    } else groups.set(key, { example: d, count: 1, requestIds: [d.request_id] });
  }
  return [...groups.values()];
}

export const KIND_LABEL: Record<string, string> = {
  status_changed: 'status changed',
  field_removed: 'field removed',
  type_changed: 'type changed',
  value_changed: 'value changed',
  field_added: 'field added',
  latency_regression: 'slower',
  request_failed: 'request failed',
};
