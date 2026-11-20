import { useCallback, useEffect, useRef, useState } from 'react';

export interface AsyncState<T> {
  data: T | undefined;
  error: Error | undefined;
  loading: boolean;
  reload: () => void;
}

/**
 * Runs `fn` whenever `key` changes and, if `pollMs` is given, again every
 * `pollMs` while `keepPolling(data)` says so. A result that arrives after the
 * component moved on (different key, or unmounted) is dropped.
 */
export function useAsync<T>(
  fn: () => Promise<T>,
  key: string,
  opts: { pollMs?: number; keepPolling?: (d: T) => boolean } = {},
): AsyncState<T> {
  const [state, setState] = useState<{ data?: T; error?: Error; loading: boolean }>({ loading: true });
  const [tick, setTick] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const optsRef = useRef(opts);
  optsRef.current = opts;

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setState((s) => ({ data: s.data, loading: true })); // keep showing the old data while reloading

    const run = () => {
      fnRef.current().then(
        (data) => {
          if (cancelled) return;
          setState({ data, loading: false });
          const { pollMs, keepPolling } = optsRef.current;
          if (pollMs && keepPolling?.(data)) timer = setTimeout(run, pollMs);
        },
        (error: Error) => {
          if (!cancelled) setState({ error, loading: false });
        },
      );
    };
    run();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [key, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data: state.data, error: state.error, loading: state.loading, reload };
}
