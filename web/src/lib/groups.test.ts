import { describe, expect, it } from 'vitest';
import { duration, showValue, timeAgo } from './format';
import { groupDiffs } from './groups';
import type { Diff } from './types';

const d = (request_id: number, over: Partial<Diff> = {}): Diff => ({
  request_id, endpoint: '/o/{id}', severity: 'breaking', kind: 'value_changed', json_path: '$.total', ...over,
});

describe('groupDiffs', () => {
  it('collapses the same problem on many requests into one row with a count', () => {
    const g = groupDiffs([d(1), d(2), d(3), d(4, { json_path: '$.other' }), d(5, { severity: 'additive' })]);
    expect(g.map((x) => [x.example.json_path, x.count])).toEqual([['$.total', 3], ['$.other', 1], ['$.total', 1]]);
    expect(g[0]!.requestIds).toEqual([1, 2, 3]);
  });
  it('filters by severity and keeps the first example', () => {
    const g = groupDiffs([d(1, { severity: 'additive' }), d(2)], 'breaking');
    expect(g).toHaveLength(1);
    expect(g[0]!.example.request_id).toBe(2);
  });
  it('caps the remembered request ids but still counts everything', () => {
    const g = groupDiffs(Array.from({ length: 50 }, (_, i) => d(i + 1)));
    expect(g[0]!.count).toBe(50);
    expect(g[0]!.requestIds).toHaveLength(20);
  });
});

describe('format', () => {
  const now = Date.parse('2026-11-20T12:00:00Z');
  it.each([
    ['2026-11-20T11:59:50Z', 'just now'],
    ['2026-11-20T11:58:00Z', '2 minutes ago'],
    ['2026-11-20T11:59:00Z', '1 minute ago'],
    ['2026-11-20T09:00:00Z', '3 hours ago'],
    ['2026-11-18T12:00:00Z', '2 days ago'],
    ['nonsense', ''],
  ])('timeAgo(%s)', (iso, want) => expect(timeAgo(iso, now)).toBe(want));

  it('duration', () => {
    expect(duration('2026-11-20T12:00:00Z', '2026-11-20T12:00:42Z')).toBe('42s');
    expect(duration('2026-11-20T12:00:00Z', '2026-11-20T12:02:05Z')).toBe('2m 5s');
    expect(duration('2026-11-20T12:00:00Z')).toBe('');
  });
  it('showValue', () => {
    expect(showValue(undefined)).toBe('—');
    expect(showValue('x')).toBe('"x"');
    expect(showValue(null)).toBe('null');
    expect(showValue({ a: 1 })).toBe('{"a":1}');
    expect(showValue('y'.repeat(100), 10)).toBe('"yyyyyyyyy…');
  });
});
