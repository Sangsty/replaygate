// Shapes of the replaygate API's JSON. Keep in sync with internal/report and internal/store.

export type Severity = 'breaking' | 'additive' | 'latency';
export type RunStatus = 'pending' | 'replaying' | 'diffing' | 'passed' | 'failed' | 'errored';

export interface Me {
  user_id: string;
  github_id: number;
  login: string;
}

export interface ProjectSummary {
  id: string;
  name: string;
  created_at: string;
}

export interface Run {
  id: string;
  commit_sha: string;
  pr_number?: number;
  status: RunStatus;
  total_requests: number;
  breaking_count: number;
  additive_count: number;
  latency_count: number;
  error?: string;
  created_at: string;
  finished_at?: string;
}

export interface Diff {
  request_id: number;
  endpoint: string;
  severity: Severity;
  kind: string;
  json_path: string;
  // Raw JSON values. They arrive already parsed; large integers may have lost precision.
  baseline_value?: unknown;
  candidate_value?: unknown;
}

export interface NoiseField {
  endpoint: string;
  json_path: string;
}

export interface RunResult {
  run: Run;
  diffs: Diff[];
  noise: NoiseField[];
}

export interface ResponseDetail {
  status: number;
  body: string;
  truncated: boolean;
  latency_ms: number;
  error?: string;
}

export interface RequestDetail {
  request: { id: number; method: string; path: string; query: string; endpoint: string };
  baseline: ResponseDetail | null;
  candidate: ResponseDetail | null;
  diffs: Diff[];
}

export const isTerminal = (s: RunStatus): boolean => s === 'passed' || s === 'failed' || s === 'errored';
