import { describe, expect, it } from 'vitest';
import { JsonError, MAX_DEPTH, normalizePath, parseJson, pathCovers, prettyLines, toLines } from './json';

const text = (src: string) => toLines(parseJson(src)).map((l) => l.text);
const paths = (src: string) => toLines(parseJson(src)).map((l) => l.path);

describe('parseJson / toLines', () => {
  it('pretty-prints with sorted keys, so key order never shows as a change', () => {
    expect(text('{"b":1,"a":{"y":true,"x":null}}')).toEqual([
      '{',
      '  "a": {',
      '    "x": null,',
      '    "y": true',
      '  },',
      '  "b": 1',
      '}',
    ]);
    expect(text('{"a":1,"b":2}')).toEqual(text('{"b":2,"a":1}'));
  });

  it('keeps numbers exactly as sent, including ones a double would round', () => {
    const lines = text('{"ts":1795167713397535516,"f":1.50,"e":1E+3,"neg":-0.0}');
    expect(lines).toContain('  "ts": 1795167713397535516'); // keys are sorted, so ts is last: no comma
    expect(lines).toContain('  "f": 1.50,');
    expect(lines).toContain('  "e": 1E+3,');
  });

  it('keeps string escapes as written', () => {
    expect(text(String.raw`{"s":"a\n\"b\" é é"}`)[1]).toBe(String.raw`  "s": "a\n\"b\" é é"`);
  });

  it('prints empty containers on one line', () => {
    expect(text('{"a":{},"b":[]}')).toEqual(['{', '  "a": {},', '  "b": []', '}']);
  });

  it('records the backend-style path of every line', () => {
    expect(paths('{"a":[{"b":1}],"x y":2}')).toEqual([
      '$', // {
      '$.a', // "a": [
      '$.a[0]', // {
      '$.a[0].b',
      '$.a[0]', // }
      '$.a', // ]
      "$['x y']", // "x y": 2   (keys sort by code unit: "a" < "x y")
      '$', // }
    ]);
  });

  it('escapes quotes in keys like the backend', () => {
    expect(paths(`{"it's":1}`)[1]).toBe("$['it\\'s']");
  });

  it('keeps the last of a repeated key', () => {
    expect(text('{"a":1,"a":2}')).toEqual(['{', '  "a": 2', '}']);
  });

  it('puts a comma after every entry but the last', () => {
    expect(text('[1,2,3]')).toEqual(['[', '  1,', '  2,', '  3', ']']);
    const lines = toLines(parseJson('[1,2]'));
    expect(lines[1]!.key).toBe('  1'); // comparison key has no comma
  });

  it.each([
    ['', 'unexpected character'],
    ['{', 'expected a string key'],
    ['{"a" 1}', "expected ':'"],
    ['{"a":1,}', 'expected a string key'],
    ['[1,]', 'unexpected character'],
    ['[1 2]', "expected ',' or ']'"],
    ['{"a":1} x', 'unexpected text'],
    ['"abc', 'unterminated'],
    ['"a\tb"', 'control character'],
    ['"\\x"', 'bad escape'],
    ['"\\u12"', 'bad \\u escape'],
    ['01', 'unexpected text'],
    ['tru', 'unexpected character'],
    ['NaN', 'unexpected character'],
  ])('rejects %j', (src, msg) => {
    expect(() => parseJson(src)).toThrow(JsonError);
    expect(() => parseJson(src)).toThrow(msg);
  });

  it('refuses documents nested deeper than the diff engine reads', () => {
    expect(() => parseJson('['.repeat(MAX_DEPTH) + ']'.repeat(MAX_DEPTH))).not.toThrow();
    expect(() => parseJson('['.repeat(MAX_DEPTH + 2) + ']'.repeat(MAX_DEPTH + 2))).toThrow('too deep');
  });
});

describe('prettyLines', () => {
  it('falls back to plain text for a body that is not JSON', () => {
    const r = prettyLines('<html>\nnope</html>');
    expect(r.isJson).toBe(false);
    expect(r.lines.map((l) => l.text)).toEqual(['<html>', 'nope</html>']);
    expect(r.lines.every((l) => l.path === '')).toBe(true);
  });
  it('reports JSON as JSON', () => expect(prettyLines('{"a":1}').isJson).toBe(true));
});

describe('normalizePath', () => {
  it.each([
    ['$.items[3].ts', '$.items[*].ts'],
    ['$.items[id=7].ts', '$.items[*].ts'],
    ['$.a[0][12]', '$.a[*][*]'],
    ["$['a.b'].c", "$['a.b'].c"],
    ["$['x=y']", "$['x=y']"],
    ['$.plain', '$.plain'],
    ['$.items[*]', '$.items[*]'],
  ])('%s → %s', (input, want) => expect(normalizePath(input)).toBe(want));
});

describe('pathCovers', () => {
  it('matches the value itself and everything inside it', () => {
    expect(pathCovers('$.address', '$.address')).toBe(true);
    expect(pathCovers('$.address', '$.address.city')).toBe(true);
    expect(pathCovers('$.items', '$.items[2].id')).toBe(true);
  });
  it('does not match siblings or prefixes of other names', () => {
    expect(pathCovers('$.name', '$.names')).toBe(false);
    expect(pathCovers('$.name', '$.nam')).toBe(false);
    expect(pathCovers('$.a.b', '$.a')).toBe(false);
  });
  it('treats array positions and keyed elements alike', () => {
    expect(pathCovers('$.items[id=7].price', '$.items[3].price')).toBe(true);
    expect(pathCovers('$.items[*]', '$.items[1]')).toBe(true);
  });
  it('never matches a response-level finding', () => expect(pathCovers('', '$')).toBe(false));
});
