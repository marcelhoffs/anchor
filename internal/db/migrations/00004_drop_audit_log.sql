-- The audit log is gone; nothing reads or writes it any more.

-- +goose Up
DROP TABLE audit_log;

-- +goose Down
CREATE TABLE audit_log (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER,
  event      TEXT NOT NULL,
  ip         TEXT,
  created_at INTEGER NOT NULL,
  actor_id   INTEGER,
  detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_created ON audit_log(created_at DESC);
