import type { RunStatus, Severity } from '../lib/types';

const STATUS_LABEL: Record<RunStatus, string> = {
  pending: 'Queued',
  replaying: 'Replaying',
  diffing: 'Comparing',
  passed: 'Passed',
  failed: 'Failed',
  errored: 'Errored',
};

export function StatusBadge({ status }: { status: RunStatus }) {
  const live = status === 'pending' || status === 'replaying' || status === 'diffing';
  return (
    <span className={`badge status-${status}`}>
      {live && <span className="pulse" aria-hidden />}
      {STATUS_LABEL[status] ?? status}
    </span>
  );
}

export function SeverityBadge({ severity }: { severity: Severity }) {
  return <span className={`badge sev-${severity}`}>{severity}</span>;
}

export function Counts({ breaking, additive, latency }: { breaking: number; additive: number; latency: number }) {
  return (
    <span className="counts">
      <span className={breaking > 0 ? 'count sev-breaking' : 'count zero'} title="Breaking">
        {breaking} breaking
      </span>
      <span className={additive > 0 ? 'count sev-additive' : 'count zero'} title="Additive">
        {additive} additive
      </span>
      <span className={latency > 0 ? 'count sev-latency' : 'count zero'} title="Latency">
        {latency} latency
      </span>
    </span>
  );
}
