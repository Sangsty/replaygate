import { Link, useParams } from 'react-router-dom';
import { Counts, StatusBadge } from '../components/Badges';
import { ErrorNote, Loading } from '../components/Status';
import { api } from '../lib/api';
import { duration, shortSha, timeAgo } from '../lib/format';
import { isTerminal } from '../lib/types';
import { useAsync } from '../lib/useAsync';

export function Runs() {
  const { pid = '' } = useParams();
  const { data, error, loading } = useAsync(() => api.runs(pid), `runs:${pid}`, {
    pollMs: 5000,
    keepPolling: (runs) => runs.some((r) => !isTerminal(r.status)),
  });
  if (loading && !data) return <Loading />;
  if (error) return <ErrorNote error={error} />;
  return (
    <section>
      <p className="crumbs">
        <Link to="/">Projects</Link> / <span>runs</span>
      </p>
      <h1>Runs</h1>
      {data && data.length === 0 ? (
        <section className="card empty">
          <p>No runs yet. They appear here when the GitHub Action (or <code>replaygate run</code>) checks a build.</p>
        </section>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Status</th>
              <th>Commit</th>
              <th>Pull request</th>
              <th>Findings</th>
              <th>Requests</th>
              <th>Started</th>
            </tr>
          </thead>
          <tbody>
            {data?.map((r) => (
              <tr key={r.id}>
                <td>
                  <Link to={`/p/${pid}/runs/${r.id}`}>
                    <StatusBadge status={r.status} />
                  </Link>
                </td>
                <td>
                  <code>{shortSha(r.commit_sha)}</code>
                </td>
                <td>{r.pr_number ? `#${r.pr_number}` : <span className="muted">—</span>}</td>
                <td>
                  <Counts breaking={r.breaking_count} additive={r.additive_count} latency={r.latency_count} />
                </td>
                <td>{r.total_requests}</td>
                <td className="muted" title={r.created_at}>
                  {timeAgo(r.created_at)}
                  {r.finished_at ? ` · took ${duration(r.created_at, r.finished_at)}` : ''}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
