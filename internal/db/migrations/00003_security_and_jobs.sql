-- Milestone 2 (2FA, API tokens, invites, admin, audit) and import/export jobs.

-- +goose Up
ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0; -- blocks TOTP code replay
ALTER TABLE users ADD COLUMN last_login_at INTEGER;

-- When this session last proved who it is (login, 2FA or re-authentication);
-- sensitive actions require it to be recent.
ALTER TABLE sessions ADD COLUMN verified_at INTEGER NOT NULL DEFAULT 0;

-- The full go-webauthn credential (flags, transports, AAGUID) as JSON.
ALTER TABLE webauthn_credentials ADD COLUMN credential TEXT NOT NULL DEFAULT '{}';
CREATE INDEX webauthn_credentials_user ON webauthn_credentials(user_id);
CREATE INDEX recovery_codes_user ON recovery_codes(user_id);
CREATE INDEX api_tokens_user ON api_tokens(user_id);

-- user_id is whom the event is about; actor_id who caused it, when that is
-- someone else (an admin).
ALTER TABLE audit_log ADD COLUMN actor_id INTEGER;
ALTER TABLE audit_log ADD COLUMN detail TEXT NOT NULL DEFAULT '';
CREATE INDEX audit_log_created ON audit_log(created_at DESC);

CREATE TABLE invites (
  id         INTEGER PRIMARY KEY,
  token_hash BLOB NOT NULL UNIQUE,           -- sha256 of the link token
  note       TEXT NOT NULL DEFAULT '',
  created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  used_by    INTEGER REFERENCES users(id) ON DELETE SET NULL
);

-- Instance-wide settings an admin can change (signup mode, 2FA policy).
CREATE TABLE instance_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

ALTER TABLE jobs ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE jobs ADD COLUMN progress INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN total INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN result TEXT NOT NULL DEFAULT '';  -- JSON summary when done
ALTER TABLE jobs ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN finished_at INTEGER;
CREATE INDEX jobs_queue ON jobs(status, run_after);
CREATE INDEX jobs_user ON jobs(user_id, id DESC);

-- +goose Down
DROP INDEX jobs_user;
DROP INDEX jobs_queue;
ALTER TABLE jobs DROP COLUMN finished_at;
ALTER TABLE jobs DROP COLUMN created_at;
ALTER TABLE jobs DROP COLUMN result;
ALTER TABLE jobs DROP COLUMN total;
ALTER TABLE jobs DROP COLUMN progress;
-- jobs.user_id stays: SQLite can't drop a column used by a foreign key, and
-- an extra nullable column is harmless to the older schema.
DROP TABLE instance_settings;
DROP TABLE invites;
DROP INDEX audit_log_created;
ALTER TABLE audit_log DROP COLUMN detail;
ALTER TABLE audit_log DROP COLUMN actor_id;
DROP INDEX api_tokens_user;
DROP INDEX recovery_codes_user;
DROP INDEX webauthn_credentials_user;
ALTER TABLE webauthn_credentials DROP COLUMN credential;
ALTER TABLE sessions DROP COLUMN verified_at;
ALTER TABLE users DROP COLUMN last_login_at;
ALTER TABLE users DROP COLUMN totp_last_step;
