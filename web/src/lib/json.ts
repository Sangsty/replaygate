// A small JSON reader and pretty-printer for the side-by-side viewer.
//
// JSON.parse is not used because it turns every number into a double: an id
// like 1795167713397535516 would be shown rounded, which in a tool that exists
// to find changed values is a lie. This parser keeps numbers and strings as
// the exact text that was sent, and it records the JSON path of every line so
// the viewer can mark lines that the diff engine reported or ignored.
//
// Paths use the backend's format (internal/diff): $.a.b, $.items[0], and
// $['key with spaces'] for keys that are not plain identifiers.

export class JsonError extends Error {}

export type JsonNode =
  | { t: 'obj'; entries: [string, JsonNode][] }
  | { t: 'arr'; items: JsonNode[] }
  | { t: 'scalar'; text: string };

export const MAX_DEPTH = 64; // same limit as the diff engine

const NUMBER = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;
const LITERALS = ['true', 'false', 'null'];

export function parseJson(src: string): JsonNode {
  let i = 0;
  const fail = (msg: string): never => {
    throw new JsonError(`${msg} at position ${i}`);
  };
  const ws = () => {
    while (i < src.length && (src[i] === ' ' || src[i] === '\n' || src[i] === '\t' || src[i] === '\r')) i++;
  };

  // Returns the raw text of a string literal, quotes included, validated.
  const str = (): string => {
    const start = i;
    i++; // opening quote
    while (i < src.length) {
      const c = src.charCodeAt(i);
      if (c === 0x22) {
        i++;
        return src.slice(start, i);
      }
      if (c < 0x20) fail('control character in string');
      if (c === 0x5c) {
        const e = src[i + 1];
        if (e === 'u') {
          if (!/^[0-9a-fA-F]{4}$/.test(src.slice(i + 2, i + 6))) fail('bad \\u escape');
          i += 6;
        } else if (e !== undefined && '"\\/bfnrt'.includes(e)) {
          i += 2;
        } else fail('bad escape');
      } else i++;
    }
    return fail('unterminated string');
  };

  const value = (depth: number): JsonNode => {
    if (depth > MAX_DEPTH) fail('nesting too deep');
    ws();
    const c = src[i];
    if (c === '{') {
      i++;
      const entries = new Map<string, JsonNode>();
      ws();
      if (src[i] === '}') {
        i++;
        return { t: 'obj', entries: [] };
      }
      for (;;) {
        ws();
        if (src[i] !== '"') fail('expected a string key');
        const key = JSON.parse(str()) as string;
        ws();
        if (src[i] !== ':') fail("expected ':'");
        i++;
        entries.set(key, value(depth + 1)); // a repeated key keeps the last value, like Go and JSON.parse
        ws();
        if (src[i] === ',') {
          i++;
          continue;
        }
        if (src[i] === '}') {
          i++;
          break;
        }
        fail("expected ',' or '}'");
      }
      // Key order carries no meaning in JSON, so sort: the same data always prints the same.
      const sorted = [...entries.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
      return { t: 'obj', entries: sorted };
    }
    if (c === '[') {
      i++;
      const items: JsonNode[] = [];
      ws();
      if (src[i] === ']') {
        i++;
        return { t: 'arr', items };
      }
      for (;;) {
        items.push(value(depth + 1));
        ws();
        if (src[i] === ',') {
          i++;
          continue;
        }
        if (src[i] === ']') {
          i++;
          break;
        }
        fail("expected ',' or ']'");
      }
      return { t: 'arr', items };
    }
    if (c === '"') return { t: 'scalar', text: str() };
    for (const lit of LITERALS) {
      if (src.startsWith(lit, i)) {
        i += lit.length;
        return { t: 'scalar', text: lit };
      }
    }
    NUMBER.lastIndex = i;
    const m = NUMBER.exec(src);
    if (m) {
      i += m[0].length;
      return { t: 'scalar', text: m[0] };
    }
    return fail('unexpected character');
  };

  const root = value(0);
  ws();
  if (i < src.length) fail('unexpected text after the value');
  return root;
}

export interface JsonLine {
  /** What to display, indentation and trailing comma included. */
  text: string;
  /** What to compare: the same without the trailing comma, so that adding or
   *  removing the last key does not mark its neighbour as changed. */
  key: string;
  /** JSON path of the value this line belongs to. */
  path: string;
  /** A scalar, the line that opens an object or array, or the one that closes it. */
  role: 'leaf' | 'open' | 'close';
}

const SIMPLE_KEY = /^[A-Za-z0-9_-]+$/;

export function childKey(parent: string, key: string): string {
  return SIMPLE_KEY.test(key) ? `${parent}.${key}` : `${parent}['${key.replace(/'/g, "\\'")}']`;
}

export function childIndex(parent: string, i: number): string {
  return `${parent}[${i}]`;
}

/** Pretty-prints a parsed document, one entry per line, remembering each line's path. */
export function toLines(root: JsonNode): JsonLine[] {
  const out: JsonLine[] = [];
  const push = (depth: number, body: string, comma: boolean, path: string, role: JsonLine['role']) => {
    const base = '  '.repeat(depth) + body;
    out.push({ text: base + (comma ? ',' : ''), key: base, path, role });
  };
  const emit = (n: JsonNode, depth: number, prefix: string, comma: boolean, path: string): void => {
    if (n.t === 'scalar') return push(depth, prefix + n.text, comma, path, 'leaf');
    if (n.t === 'obj') {
      if (n.entries.length === 0) return push(depth, prefix + '{}', comma, path, 'leaf');
      push(depth, prefix + '{', false, path, 'open');
      n.entries.forEach(([k, v], idx) =>
        emit(v, depth + 1, JSON.stringify(k) + ': ', idx < n.entries.length - 1, childKey(path, k)),
      );
      return push(depth, '}', comma, path, 'close');
    }
    if (n.items.length === 0) return push(depth, prefix + '[]', comma, path, 'leaf');
    push(depth, prefix + '[', false, path, 'open');
    n.items.forEach((v, idx) => emit(v, depth + 1, '', idx < n.items.length - 1, childIndex(path, idx)));
    return push(depth, ']', comma, path, 'close');
  };
  emit(root, 0, '', false, '$');
  return out;
}

/** Parses and pretty-prints. A body that is not JSON is shown as plain text lines. */
export function prettyLines(body: string): { lines: JsonLine[]; isJson: boolean } {
  try {
    return { lines: toLines(parseJson(body)), isJson: true };
  } catch (e) {
    if (!(e instanceof JsonError)) throw e;
    const lines = body.split('\n').map((l): JsonLine => ({ text: l, key: l, path: '', role: 'leaf' }));
    return { lines, isJson: false };
  }
}

/**
 * Rewrites concrete array selectors to [*], exactly as the backend does when it
 * stores learned noise: [3] and [id=7] become [*]; quoted keys are left alone.
 */
export function normalizePath(path: string): string {
  let out = '';
  for (let i = 0; i < path.length; i++) {
    if (path[i] === '[') {
      const j = path.indexOf(']', i);
      if (j > i) {
        const inner = path.slice(i + 1, j);
        if (/^[+-]?\d+$/.test(inner) || (inner.includes('=') && !inner.startsWith("'"))) {
          out += '[*]';
          i = j;
          continue;
        }
      }
    }
    out += path[i];
  }
  return out;
}

/** True if `line` is the value at `target` or anything inside it. Both sides are normalised. */
export function pathCovers(target: string, line: string): boolean {
  if (target === '') return false; // response-level findings (status, latency) have no line
  const t = normalizePath(target);
  const l = normalizePath(line);
  return l === t || l.startsWith(t + '.') || l.startsWith(t + '[');
}

/**
 * Makes lines inside ignored (noise) fields compare equal whatever their
 * values are. Without this, a noise field whose value differs between the two
 * bodies (which is what makes it noise) would be aligned against unrelated
 * lines and drag real differences out of place.
 */
export function neutraliseNoise(lines: JsonLine[], noisePaths: string[]): JsonLine[] {
  if (noisePaths.length === 0) return lines;
  return lines.map((l) =>
    l.path && noisePaths.some((p) => pathCovers(p, l.path))
      ? { ...l, key: `\u0000noise|${normalizePath(l.path)}|${l.role}` }
      : l,
  );
}
