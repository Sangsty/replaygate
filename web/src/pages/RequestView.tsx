import { Link, useParams } from 'react-router-dom';
import { SeverityBadge } from '../components/Badges';
import { DiffViewer } from '../components/DiffViewer';
import { ErrorNote, Loading } from '../components/Status';
import { api } from '../lib/api';
import { KIND_LABEL } from '../lib/groups';
import { showValue } from '../lib/format';
import { useAsync } from '../lib/useAsync';

export function RequestView() {
  const { pid = '', rid = '', reqid = '' } = useParams();
  const detail = useAsync(() => api.requestDetail(pid, rid, Number(reqid)), `req:${pid}:${rid}:${reqid}`);
  // The run's noise list tells the viewer which differing lines were deliberately ignored.
  const run = useAsync(() => api.run(pid, rid, 0), `run0:${pid}:${rid}`);

  if ((detail.loading && !detail.data) || (run.loading && !run.data)) return <Loading />;
  if (detail.error) return <ErrorNote error={detail.error} />;
  if (!detail.data) return null;
  const d = detail.data;

  return (
    <section>
      <p className="crumbs">
        <Link to="/">Projects</Link> / <Link to={`/p/${pid}`}>runs</Link> / <Link to={`/p/${pid}/runs/${rid}`}>run</Link> /{' '}
        <span>request {d.request.id}</span>
      </p>
      <h1>
        <code>
          {d.request.method} {d.request.path}
          {d.request.query ? `?${d.request.query}` : ''}
        </code>
      </h1>
      <p className="muted">
        Endpoint <code>{d.request.endpoint}</code>
      </p>

      {d.diffs.length > 0 ? (
        <ul className="finding-list">
          {d.diffs.map((x) => (
            <li key={x.kind + x.json_path}>
              <SeverityBadge severity={x.severity} /> {KIND_LABEL[x.kind] ?? x.kind} <code>{x.json_path || '(response)'}</code>{' '}
              <span className="muted">
                <code>{showValue(x.baseline_value)}</code> → <code>{showValue(x.candidate_value)}</code>
              </span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">No findings for this request.</p>
      )}

      <DiffViewer
        endpoint={d.request.endpoint}
        baseline={d.baseline}
        candidate={d.candidate}
        diffs={d.diffs}
        noise={run.data?.noise ?? []}
      />
    </section>
  );
}
