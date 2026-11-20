import { describe, expect, it } from 'vitest';
import type { JsonLine } from './json';
import { toLines, parseJson, neutraliseNoise } from './json';
import { align, myers } from './linediff';

const L = (s: string): JsonLine => ({ text: s, key: s, path: '', role: 'leaf' });
const lines = (...s: string[]) => s.map(L);
const kinds = (a: string[], b: string[]) => align(lines(...a), lines(...b)).rows.map((r) => r.kind);

describe('align', () => {
  it('marks identical documents as all same', () => {
    expect(kinds(['a', 'b'], ['a', 'b'])).toEqual(['same', 'same']);
    expect(align([], []).rows).toEqual([]);
  });

  it('pairs a replaced line as one changed row', () => {
    const { rows } = align(lines('a', 'x', 'c'), lines('a', 'y', 'c'));
    expect(rows.map((r) => r.kind)).toEqual(['same', 'changed', 'same']);
    expect(rows[1]!.left!.text).toBe('x');
    expect(rows[1]!.right!.text).toBe('y');
  });

  it('shows a removed line on the left only, and an added line on the right only', () => {
    const removed = align(lines('a', 'b', 'c'), lines('a', 'c')).rows;
    expect(removed.map((r) => r.kind)).toEqual(['same', 'removed', 'same']);
    expect(removed[1]!.right).toBeNull();
    const added = align(lines('a', 'c'), lines('a', 'b', 'c')).rows;
    expect(added.map((r) => r.kind)).toEqual(['same', 'added', 'same']);
    expect(added[1]!.left).toBeNull();
  });

  it('handles one side empty', () => {
    expect(kinds([], ['a', 'b'])).toEqual(['added', 'added']);
    expect(kinds(['a'], [])).toEqual(['removed']);
  });

  it('pairs uneven replacements and leaves the rest as removed/added', () => {
    expect(kinds(['a', 'x1', 'x2', 'x3', 'z'], ['a', 'y', 'z'])).toEqual(['same', 'changed', 'removed', 'removed', 'same']);
  });

  it('ignores a trailing comma when comparing, so a dropped last key does not touch its neighbour', () => {
    const left: JsonLine[] = [
      { text: '  "a": 1,', key: '  "a": 1', path: '$.a', role: 'leaf' },
      { text: '  "b": 2', key: '  "b": 2', path: '$.b', role: 'leaf' },
    ];
    const right: JsonLine[] = [{ text: '  "a": 1', key: '  "a": 1', path: '$.a', role: 'leaf' }];
    expect(align(left, right).rows.map((r) => r.kind)).toEqual(['same', 'removed']);
  });

  it('degrades, instead of hanging, when the documents are very different', () => {
    const a = Array.from({ length: 300 }, (_, i) => `a${i}`);
    const b = Array.from({ length: 300 }, (_, i) => `b${i}`);
    const r = align(lines(...a), lines(...b), 50);
    expect(r.degraded).toBe(true);
    expect(r.rows).toHaveLength(300);
    expect(r.rows.every((x) => x.kind === 'changed')).toBe(true);
  });
});

describe('myers', () => {
  // Apply the edit script to `a`; the result must be `b`, and the script must be minimal.
  const apply = (a: string[], b: string[], ops: ReturnType<typeof myers>) => {
    const out: string[] = [];
    let i = 0;
    let j = 0;
    for (const op of ops!) {
      if (op === 'eq') {
        expect(a[i]).toBe(b[j]);
        out.push(a[i++]!);
        j++;
      } else if (op === 'del') i++;
      else out.push(b[j++]!);
    }
    expect(i).toBe(a.length);
    expect(j).toBe(b.length);
    return out;
  };
  const lcs = (a: string[], b: string[]) => {
    const t = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
    for (let i = 1; i <= a.length; i++)
      for (let j = 1; j <= b.length; j++)
        t[i]![j] = a[i - 1] === b[j - 1] ? t[i - 1]![j - 1]! + 1 : Math.max(t[i - 1]![j]!, t[i]![j - 1]!);
    return t[a.length]![b.length]!;
  };

  it('produces a valid, minimal edit script for random inputs', () => {
    let seed = 12345;
    const rnd = (n: number) => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) % n);
    for (let round = 0; round < 400; round++) {
      const a = Array.from({ length: rnd(14) }, () => 'abcd'[rnd(4)]!);
      const b = Array.from({ length: rnd(14) }, () => 'abcd'[rnd(4)]!);
      const ops = myers(a, b, 100)!;
      expect(apply(a, b, ops)).toEqual(b);
      const edits = ops.filter((o) => o !== 'eq').length;
      expect(edits).toBe(a.length + b.length - 2 * lcs(a, b)); // minimal
    }
  });

  it('returns null when more edits are needed than allowed', () => {
    expect(myers(['a', 'b', 'c'], ['x', 'y', 'z'], 2)).toBeNull();
    expect(myers(['a', 'b', 'c'], ['x', 'y', 'z'], 6)).not.toBeNull();
  });
});

describe('noise fields in the viewer', () => {
  const doc = (s: string) => toLines(parseJson(s));
  it('line up by name, not by value, so they cannot drag real differences out of place', () => {
    const base = doc('{"etag":"e111","id":2,"total":20}');
    const cand = doc('{"currency":"USD","etag":"e999","id":2,"total":99}');
    const rows = align(neutraliseNoise(base, ['$.etag']), neutraliseNoise(cand, ['$.etag'])).rows;
    expect(rows.map((r) => [r.kind, r.right?.text.trim() ?? ''])).toEqual([
      ['same', '{'],
      ['added', '"currency": "USD",'],
      ['same', '"etag": "e999",'], // different value, same row, not a difference
      ['same', '"id": 2,'],
      ['changed', '"total": 99'],
      ['same', '}'],
    ]);
  });
  it('treats a noisy object as one block, however its contents differ', () => {
    const rows = align(
      neutraliseNoise(doc('{"meta":{"a":1,"b":2},"x":1}'), ['$.meta']),
      neutraliseNoise(doc('{"meta":{"a":5,"b":6},"x":1}'), ['$.meta']),
    ).rows;
    expect(rows.every((r) => r.kind === 'same')).toBe(true);
  });
  it('leaves everything alone when nothing is noisy', () => {
    const l = doc('{"a":1}');
    expect(neutraliseNoise(l, [])).toBe(l);
  });
});
