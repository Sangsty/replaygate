-- 0001_init: core schema.
-- UUIDs for anything that appears in a URL; bigint identity for high-volume tables.
-- TEXT + CHECK instead of enums: easier to evolve with later migrations.

CREATE TABLE users (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  github_id  bigint NOT NULL UNIQUE,
  login      text NOT NULL,
  avatar_url text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name           text NOT NULL,
  sanitize_rules jsonb NOT NULL DEFAULT
    '{"headers":["authorization","cookie","set-cookie","x-api-key"],"query_params":[],"json_paths":[]}',
  diff_config    jsonb NOT NULL DEFAULT
    '{"float_tolerance":1e-9,"array_modes":{},"latency":{"ratio":1.5,"min_delta_ms":50},"fail_on":["breaking"]}',
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner_id, name)
);

-- sha256 is enough for key_hash: keys are 256-bit random values, so no slow KDF is needed.
CREATE TABLE api_keys (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name         text NOT NULL,
  prefix       text NOT NULL,
  key_hash     bytea NOT NULL UNIQUE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at   timestamptz
);

CREATE TABLE recorded_requests (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  fingerprint   bytea NOT NULL,
  method        text NOT NULL CHECK (method = 'GET'),
  path          text NOT NULL,
  endpoint      text NOT NULL,
  query         text NOT NULL DEFAULT '',
  headers       jsonb NOT NULL DEFAULT '{}',
  body          bytea,
  status        integer NOT NULL,
  latency_ms    integer NOT NULL,
  hit_count     integer NOT NULL DEFAULT 1,
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, fingerprint)
);
CREATE INDEX recorded_requests_endpoint_idx ON recorded_requests (project_id, endpoint);

CREATE TABLE replay_runs (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  commit_sha     text NOT NULL,
  pr_number      integer,
  status         text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','replaying','diffing','passed','failed','errored')),
  config         jsonb NOT NULL DEFAULT '{}',
  total_requests integer NOT NULL DEFAULT 0,
  breaking_count integer NOT NULL DEFAULT 0,
  additive_count integer NOT NULL DEFAULT 0,
  latency_count  integer NOT NULL DEFAULT 0,
  error          text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  finished_at    timestamptz
);
CREATE INDEX replay_runs_project_idx ON replay_runs (project_id, created_at DESC);

CREATE TABLE replay_responses (
  run_id     uuid   NOT NULL REFERENCES replay_runs(id) ON DELETE CASCADE,
  request_id bigint NOT NULL REFERENCES recorded_requests(id) ON DELETE CASCADE,
  variant    text   NOT NULL CHECK (variant IN ('baseline_a','baseline_b','candidate')),
  status     integer,
  body       bytea,
  truncated  boolean NOT NULL DEFAULT false,
  latency_ms integer,
  error      text,
  PRIMARY KEY (run_id, request_id, variant)
);

-- Learned per run, so a field that was noisy once cannot hide regressions forever.
CREATE TABLE noise_fields (
  run_id    uuid NOT NULL REFERENCES replay_runs(id) ON DELETE CASCADE,
  endpoint  text NOT NULL,
  json_path text NOT NULL,
  PRIMARY KEY (run_id, endpoint, json_path)
);

CREATE TABLE diffs (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  run_id          uuid   NOT NULL REFERENCES replay_runs(id) ON DELETE CASCADE,
  request_id      bigint NOT NULL REFERENCES recorded_requests(id) ON DELETE CASCADE,
  endpoint        text   NOT NULL,
  severity        text   NOT NULL CHECK (severity IN ('breaking','additive','latency')),
  kind            text   NOT NULL CHECK (kind IN
                  ('status_changed','field_removed','type_changed','value_changed',
                   'field_added','latency_regression','request_failed')),
  json_path       text   NOT NULL DEFAULT '',
  baseline_value  jsonb,
  candidate_value jsonb,
  UNIQUE (run_id, request_id, kind, json_path)
);
CREATE INDEX diffs_run_idx ON diffs (run_id, severity, endpoint);

CREATE TABLE jobs (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id       uuid   NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  run_id           uuid   NOT NULL REFERENCES replay_runs(id) ON DELETE CASCADE,
  kind             text   NOT NULL CHECK (kind IN ('replay_batch','diff_run')),
  payload          jsonb  NOT NULL,
  status           text   NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','dead')),
  attempts         integer NOT NULL DEFAULT 0,
  max_attempts     integer NOT NULL DEFAULT 5,
  run_after        timestamptz NOT NULL DEFAULT now(),
  lease_expires_at timestamptz,
  locked_by        text,
  last_error       text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  finished_at      timestamptz
);
CREATE INDEX jobs_claim_idx ON jobs (run_after) WHERE status IN ('queued','running');
