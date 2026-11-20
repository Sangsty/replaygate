import type { Me, ProjectSummary, RequestDetail, Run, RunResult } from './types';

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: 'same-origin', headers: { Accept: 'application/json' }, ...init });
  if (res.status === 204) return undefined as T;
  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    /* not JSON: the status below is the message */
  }
  if (!res.ok) {
    const msg = (body as { error?: string } | null)?.error ?? `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
  return body as T;
}

export const api = {
  me: () => request<Me>('/v1/me'),
  projects: () => request<{ projects: ProjectSummary[] }>('/v1/projects').then((r) => r.projects),
  runs: (pid: string, limit = 50) =>
    request<{ runs: Run[] }>(`/v1/projects/${pid}/runs?limit=${limit}`).then((r) => r.runs),
  run: (pid: string, rid: string, diffLimit = 1000) =>
    request<RunResult>(`/v1/projects/${pid}/runs/${rid}?diff_limit=${diffLimit}`),
  requestDetail: (pid: string, rid: string, reqId: number) =>
    request<RequestDetail>(`/v1/projects/${pid}/runs/${rid}/requests/${reqId}`),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
};
