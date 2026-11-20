-- 0002_project_members: who may open a project in the dashboard.
--
-- Members are recorded by GitHub login, lower-cased, so an owner can grant
-- access before the person has ever signed in. A project's owner (projects.owner_id)
-- always has access too. Logins can be renamed and later reused on GitHub; for
-- this MVP that small window is accepted, and removing a member row closes it.

CREATE TABLE project_members (
  project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  github_login text NOT NULL CHECK (github_login = lower(github_login) AND github_login ~ '^[a-z0-9](?:[a-z0-9-]{0,38})$'),
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, github_login)
);
CREATE INDEX project_members_login_idx ON project_members (github_login);
