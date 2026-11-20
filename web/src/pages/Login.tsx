const MESSAGES: Record<string, string> = {
  denied: 'GitHub sign-in was cancelled.',
  state: 'That sign-in attempt could not be verified (it may have expired). Please try again.',
  failed: 'GitHub sign-in failed. Please try again.',
};

export function Login({ error }: { error: string | null }) {
  return (
    <section className="card login">
      <h1>Replay Gate</h1>
      <p>Catch breaking API changes before they merge. Sign in to see your projects and runs.</p>
      {error && (
        <p className="note warn" role="alert">
          {MESSAGES[error] ?? MESSAGES.failed}
        </p>
      )}
      {/* A plain link, not fetch: the browser has to follow the redirect to GitHub. */}
      <a className="button" href="/auth/github/login">
        Sign in with GitHub
      </a>
      <p className="muted small">Only your public GitHub profile is read. No repository access is requested.</p>
    </section>
  );
}
