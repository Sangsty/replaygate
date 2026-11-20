import { ApiError } from '../lib/api';

export const Loading = () => <p className="muted" role="status">Loading…</p>;

export function ErrorNote({ error }: { error: Error }) {
  const hint =
    error instanceof ApiError && error.status === 404
      ? 'It may not exist, or you may not have access to it.'
      : error instanceof ApiError && error.status === 401
        ? 'Your session has ended. Reload the page to sign in again.'
        : '';
  return (
    <div className="note warn" role="alert">
      <strong>Could not load this.</strong> {error.message} {hint}
    </div>
  );
}
