-- Single sign-on through an OpenID Connect provider (issue #66).

-- +goose Up

-- An identity at the provider, (issuer, subject), signs in as one user. A
-- user has at most one. Never matched by email or name, only by subject.
CREATE TABLE oidc_identities (
  issuer       TEXT NOT NULL,
  subject      TEXT NOT NULL,
  user_id      INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  PRIMARY KEY (issuer, subject)
) WITHOUT ROWID;

-- Sessions started through the provider, which does its own second factor.
ALTER TABLE sessions ADD COLUMN sso INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN sso;
DROP TABLE oidc_identities;
