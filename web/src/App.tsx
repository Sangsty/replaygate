import { useCallback, useEffect } from 'react';
import { BrowserRouter, Link, Route, Routes, useSearchParams } from 'react-router-dom';
import { Loading } from './components/Status';
import { ApiError, api } from './lib/api';
import { useAsync } from './lib/useAsync';
import { Login } from './pages/Login';
import { Projects } from './pages/Projects';
import { RequestView } from './pages/RequestView';
import { RunDetail } from './pages/RunDetail';
import { Runs } from './pages/Runs';

export default function App() {
  return (
    <BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
      <Shell />
    </BrowserRouter>
  );
}

function Shell() {
  const me = useAsync(api.me, 'me');
  const [params, setParams] = useSearchParams();
  const loginError = params.get('login_error');

  // Drop ?login_error from the address once it has been read, so a reload does not repeat it.
  useEffect(() => {
    if (loginError && me.data) {
      params.delete('login_error');
      setParams(params, { replace: true });
    }
  }, [loginError, me.data, params, setParams]);

  const signOut = useCallback(async () => {
    await api.logout();
    window.location.assign('/');
  }, []);

  if (me.loading && !me.data) return <main className="page"><Loading /></main>;

  const signedOut = me.error instanceof ApiError && me.error.status === 401;
  if (signedOut) {
    return (
      <main className="page narrow">
        <Login error={loginError} />
      </main>
    );
  }
  if (me.error) {
    return (
      <main className="page">
        <p className="note warn" role="alert">Could not reach the server: {me.error.message}</p>
      </main>
    );
  }

  return (
    <>
      <header className="topbar">
        <Link to="/" className="brand">Replay Gate</Link>
        <span className="spacer" />
        <span className="muted">{me.data?.login}</span>
        <button className="link" onClick={signOut}>Sign out</button>
      </header>
      <main className="page">
        <Routes>
          <Route path="/" element={<Projects />} />
          <Route path="/p/:pid" element={<Runs />} />
          <Route path="/p/:pid/runs/:rid" element={<RunDetail />} />
          <Route path="/p/:pid/runs/:rid/r/:reqid" element={<RequestView />} />
          <Route path="*" element={<p className="note">That page does not exist. <Link to="/">Back to projects</Link></p>} />
        </Routes>
      </main>
    </>
  );
}
