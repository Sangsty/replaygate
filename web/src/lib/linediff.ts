// Line alignment for the side-by-side viewer, using Myers' O(ND) algorithm.
// It is fast when the two documents are mostly the same, which is the normal
// case for a replay diff, and it gives up on documents that differ a lot
// instead of using unbounded time or memory.

import type { JsonLine } from './json';

export type RowKind = 'same' | 'changed' | 'removed' | 'added';

export interface Row {
  kind: RowKind;
  left: JsonLine | null;
  right: JsonLine | null;
}

export interface Alignment {
  rows: Row[];
  /** True when the documents were too different to align; rows are then paired by position. */
  degraded: boolean;
}

export const MAX_EDITS = 2000;
export const MAX_LINES = 50_000;

type Op = 'eq' | 'del' | 'ins';

/** Shortest edit script from a to b, or null if it needs more than maxD edits. */
export function myers(a: string[], b: string[], maxD: number): Op[] | null {
  const n = a.length;
  const m = b.length;
  if (n === 0 && m === 0) return [];
  const max = Math.min(n + m, maxD);
  const off = max + 1;
  const v = new Int32Array(2 * max + 3);
  const trace: Int32Array[] = [];

  for (let d = 0; d <= max; d++) {
    // Snapshot of v as it was after step d-1: backtracking reads it.
    trace.push(v.slice(off - d - 1, off + d + 2));
    for (let k = -d; k <= d; k += 2) {
      let x: number;
      if (k === -d || (k !== d && v[off + k - 1]! < v[off + k + 1]!)) x = v[off + k + 1]!;
      else x = v[off + k - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      v[off + k] = x;
      if (x >= n && y >= m) return backtrack(a, b, trace, d);
    }
  }
  return null;
}

function backtrack(a: string[], b: string[], trace: Int32Array[], dEnd: number): Op[] {
  const ops: Op[] = [];
  let x = a.length;
  let y = b.length;
  for (let d = dEnd; d > 0; d--) {
    const v = trace[d]!; // covers k in [-d-1, d+1]; index = k + d + 1
    const at = (k: number) => v[k + d + 1]!;
    const k = x - y;
    const prevK = k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
    const prevX = at(prevK);
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      ops.push('eq');
      x--;
      y--;
    }
    if (x === prevX) {
      ops.push('ins');
      y--;
    } else {
      ops.push('del');
      x--;
    }
  }
  while (x > 0 && y > 0) {
    ops.push('eq');
    x--;
    y--;
  }
  return ops.reverse();
}

/** Aligns two documents into rows for display. */
export function align(left: JsonLine[], right: JsonLine[], maxEdits = MAX_EDITS): Alignment {
  const ops =
    left.length + right.length > MAX_LINES
      ? null
      : myers(
          left.map((l) => l.key),
          right.map((l) => l.key),
          maxEdits,
        );
  if (!ops) return { rows: pairByPosition(left, right), degraded: true };

  const rows: Row[] = [];
  let li = 0;
  let ri = 0;
  let dels: JsonLine[] = [];
  let inss: JsonLine[] = [];
  const flush = () => {
    // A run of removals followed by additions is a change: show them on the same rows.
    const n = Math.max(dels.length, inss.length);
    for (let i = 0; i < n; i++) {
      const l = dels[i] ?? null;
      const r = inss[i] ?? null;
      rows.push({ kind: l && r ? 'changed' : l ? 'removed' : 'added', left: l, right: r });
    }
    dels = [];
    inss = [];
  };
  for (const op of ops) {
    if (op === 'eq') {
      flush();
      rows.push({ kind: 'same', left: left[li++]!, right: right[ri++]! });
    } else if (op === 'del') dels.push(left[li++]!);
    else inss.push(right[ri++]!);
  }
  flush();
  return { rows, degraded: false };
}

function pairByPosition(left: JsonLine[], right: JsonLine[]): Row[] {
  const rows: Row[] = [];
  const n = Math.max(left.length, right.length);
  for (let i = 0; i < n; i++) {
    const l = left[i] ?? null;
    const r = right[i] ?? null;
    rows.push({
      kind: l && r ? (l.key === r.key ? 'same' : 'changed') : l ? 'removed' : 'added',
      left: l,
      right: r,
    });
  }
  return rows;
}
