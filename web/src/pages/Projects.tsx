import { Link } from 'react-router-dom';
import { api } from '../lib/api';
import { timeAgo } from '../lib/format';
import { useAsync } from '../lib/useAsync';
import { ErrorNote, Loading } from '../components/Status';

export function Projects() {
  const { data, error, loading } = useAsync(api.projects, 'projects');
  if (loading && !data) return <Loading />;
  if (error) return <ErrorNote error={error} />;
  if (!data || data.length === 0) {
    return (
      <section className="card empty">
        <h2>No projects yet</h2>
        <p>
          You are signed in, but you have not been added to a project. Ask the person who runs this server to create one
          for you:
        </p>
        <pre>make bootstrap NAME=my-service OWNER=your-github-login</pre>
      </section>
    );
  }
  return (
    <section>
      <h1>Projects</h1>
      <ul className="list">
        {data.map((p) => (
          <li key={p.id}>
            <Link to={`/p/${p.id}`} className="row-link">
              <strong>{p.name}</strong>
              <span className="muted">created {timeAgo(p.created_at)}</span>
            </Link>
          </li>
        ))}
      </ul>
    </section>
  );
}
