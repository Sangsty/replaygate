import { useMemo } from 'react';
import { neutraliseNoise, pathCovers, prettyLines, type JsonLine } from '../lib/json';
import { align, type Row } from '../lib/linediff';
import type { Diff, NoiseField, ResponseDetail } from '../lib/types';

type Mark = 'finding' | 'noise' | '';

interface Props {
  endpoint: string;
  baseline: ResponseDetail | null;
  candidate: ResponseDetail | null;
  diffs: Diff[];
  noise: NoiseField[];
}

/**
 * Baseline and candidate bodies side by side. Rows that differ are tinted;
 * lines the diff engine reported get a coloured edge, and lines it ignored as
 * noise are greyed out so a visible difference that is not a finding is
 * explained rather than puzzling.
 */
export function DiffViewer({ endpoint, baseline, candidate, diffs, noise }: Props) {
  const findingPaths = useMemo(() => diffs.filter((d) => d.severity !== 'latency').map((d) => d.json_path), [diffs]);
  const noisePaths = useMemo(() => noise.filter((n) => n.endpoint === endpoint).map((n) => n.json_path), [noise, endpoint]);

  const view = useMemo(() => {
    const left = baseline ? prettyLines(baseline.body) : null;
    const right = candidate ? prettyLines(candidate.body) : null;
    const a = align(neutraliseNoise(left?.lines ?? [], noisePaths), neutraliseNoise(right?.lines ?? [], noisePaths));
    return { a, plain: (left && !left.isJson) || (right && !right.isJson) };
  }, [baseline, candidate, noisePaths]);

  const mark = (line: JsonLine | null): Mark => {
    if (!line || !line.path) return '';
    if (findingPaths.some((p) => pathCovers(p, line.path))) return 'finding';
    if (noisePaths.some((p) => pathCovers(p, line.path))) return 'noise';
    return '';
  };

  return (
    <div className="viewer">
      {(baseline?.truncated || candidate?.truncated) && (
        <p className="note warn">
          A body was longer than the replay limit and was cut off, so lines at the end may be missing from this view.
        </p>
      )}
      {view.plain && <p className="note">A body is not JSON, so it is shown as plain text.</p>}
      {view.a.degraded && (
        <p className="note warn">The two bodies are too different to line up, so they are paired by position.</p>
      )}
      <div className="viewer-head">
        <div>
          Baseline <ResponseMeta r={baseline} />
        </div>
        <div>
          This PR <ResponseMeta r={candidate} />
        </div>
      </div>
      <div className="viewer-body" role="table" aria-label="Side-by-side response bodies">
        {view.a.rows.length === 0 && <p className="note">Neither build returned a body.</p>}
        {view.a.rows.map((row, i) => (
          <ViewerRow key={i} n={i + 1} row={row} leftMark={mark(row.left)} rightMark={mark(row.right)} />
        ))}
      </div>
    </div>
  );
}

function ResponseMeta({ r }: { r: ResponseDetail | null }) {
  if (!r) return <span className="muted">no response</span>;
  if (r.error) return <span className="sev-breaking">{r.error}</span>;
  return (
    <span className="muted">
      HTTP {r.status} · {r.latency_ms} ms
    </span>
  );
}

function ViewerRow({ n, row, leftMark, rightMark }: { n: number; row: Row; leftMark: Mark; rightMark: Mark }) {
  const kind = row.kind;
  return (
    <div className={`vrow ${kind}`} role="row">
      <span className="ln" aria-hidden>
        {n}
      </span>
      <Cell line={row.left} mark={leftMark} side="left" kind={kind} />
      <Cell line={row.right} mark={rightMark} side="right" kind={kind} />
    </div>
  );
}

function Cell({ line, mark, side, kind }: { line: JsonLine | null; mark: Mark; side: 'left' | 'right'; kind: string }) {
  const empty = line === null;
  return (
    <code
      role="cell"
      className={`vcell ${side} ${empty ? 'empty' : ''} ${mark ? 'mark-' + mark : ''}`}
      title={mark === 'noise' ? 'Ignored: this field changes between replays of the same build' : mark === 'finding' ? 'Reported as a finding' : undefined}
      data-kind={kind}
    >
      {line ? line.text : ''}
    </code>
  );
}
