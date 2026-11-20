import { useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { Counts, SeverityBadge, StatusBadge } from '../components/Badges';
import { ErrorNote, Loading } from '../components/Status';
import { api } from '../lib/api';
import { duration, shortSha, showValue, timeAgo } from '../lib/format';
import { groupDiffs, KIND_LABEL } from '../lib/groups';
import { isTerminal, type Severity } from '../lib/types';
import { useAsync } from '../lib/useAsync';

const SEVERITIES: Severity[] = ['breaking', 'additive', 'latency'];

export function RunDetail() {
  const { pid = '', rid = '' } = useParams();
  const { data, error, loading } = useAsync(() => api.run(pid, rid), `run:${pid}:${rid}`, {
    pollMs: 3000,
    keepPolling: (d) => !isTerminal(d.run.status),
  });
  const [only, setOnly] = useState<Severity | 'all'>('all');
  const groups = useMemo(() => groupDiffs(data?.diffs ?? [], only === 'all' ? undefined : only), [data, only]);

  if (loading && !data) return <Loading />;
  if (error) return <ErrorNote error={error} />;
  if (!data) return null;
  const { run, noise } = data;
  const shown = data.diffs.length;
  const total = run.breaking_count + run.additive_count + run.latency_count;

  return (
    <section>
      <p className="crumbs">
        <Link to="/">Projects</Link> / <Link to={`/p/${pid}`}>runs</Link> / <span>{shortSha(run.id)}</span>
      </p>
      <header className="run-head">
        <h1>
          <StatusBadge status={run.status} /> commit <code>{shortSha(run.commit_sha)}</code>
          {run.pr_number ? <span className="muted"> · PR #{run.pr_number}</span> : null}
        </h1>
        <p className="muted">
          {run.total_requests} requests replayed · started {timeAgo(run.created_at)}
          {run.finished_at ? ` · took ${duration(run.created_at, run.finished_at)}` : ''}
        </p>
        <Counts breaking={run.breaking_count} additive={run.additive_count} latency={run.latency_count} />
      </header>

      {run.status === 'errored' && (
        <div className="note warn" role="alert">
          <strong>The run could not finish</strong>, so it says nothing about this change.
          {run.error ? <> {run.error}</> : null}
        </div>
      )}
      {!isTerminal(run.status) && <p className="muted" role="status">Still running. This page refreshes by itself.</p>}

      {total > 0 && (
        <>
          <div className="chips" role="group" aria-label="Filter by severity">
            <button className={only === 'all' ? 'chip on' : 'chip'} onClick={() => setOnly('all')}>
              All ({total})
            </button>
            {SEVERITIES.map((s) => {
              const n = s === 'breaking' ? run.breaking_count : s === 'additive' ? run.additive_count : run.latency_count;
              return (
                <button key={s} className={only === s ? 'chip on' : 'chip'} disabled={n === 0} onClick={() => setOnly(s)}>
                  {s} ({n})
                </button>
              );
            })}
          </div>

          <table className="table findings">
            <thead>
              <tr>
                <th>Severity</th>
                <th>Endpoint</th>
                <th>Change</th>
                <th>Path</th>
                <th>Baseline → PR</th>
                <th>Requests</th>
              </tr>
            </thead>
            <tbody>
              {groups.map((g) => (
                <tr key={`${g.example.severity}|${g.example.endpoint}|${g.example.kind}|${g.example.json_path}`}>
                  <td>
                    <SeverityBadge severity={g.example.severity} />
                  </td>
                  <td>
                    <code>{g.example.endpoint}</code>
                  </td>
                  <td>{KIND_LABEL[g.example.kind] ?? g.example.kind}</td>
                  <td>
                    <code>{g.example.json_path || '(response)'}</code>
                  </td>
                  <td className="values">
                    <code>{showValue(g.example.baseline_value)}</code> → <code>{showValue(g.example.candidate_value)}</code>
                  </td>
                  <td>
                    <Link to={`/p/${pid}/runs/${rid}/r/${g.example.request_id}`} title="Compare the responses side by side">
                      {g.count === 1 ? 'compare' : `${g.count} · compare first`}
                    </Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {shown < total && (
            <p className="muted">
              Showing the {shown} most severe of {total} findings.
            </p>
          )}
        </>
      )}
      {isTerminal(run.status) && run.status !== 'errored' && total === 0 && (
        <section className="card empty">
          <p>No differences found. The PR answered all {run.total_requests} replayed requests like the baseline.</p>
        </section>
      )}

      {noise.length > 0 && (
        <details className="card">
          <summary>Ignored as noise ({noise.length} fields)</summary>
          <p className="muted">These fields differed between two replays of the same baseline build, so they were not compared.</p>
          <ul>
            {noise.map((n) => (
              <li key={n.endpoint + n.json_path}>
                <code>{n.endpoint}</code> <code>{n.json_path}</code>
              </li>
            ))}
          </ul>
        </details>
      )}
    </section>
  );
}
