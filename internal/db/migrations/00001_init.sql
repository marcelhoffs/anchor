-- Anchor's schema. Earlier migrations were squashed into this one before the
-- first public release; new migrations start at 00002.

-- +goose Up

-- Accounts --------------------------------------------------------------

CREATE TABLE users (
  id             INTEGER PRIMARY KEY,
  username       TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash  TEXT NOT NULL,                       -- argon2id encoded
  is_admin       INTEGER NOT NULL DEFAULT 0,
  is_active      INTEGER NOT NULL DEFAULT 1,
  totp_secret    BLOB,                                -- encrypted (AES-GCM, key derived from ANCHOR_SECRET_KEY)
  totp_enabled   INTEGER NOT NULL DEFAULT 0,
  totp_last_step INTEGER NOT NULL DEFAULT 0,          -- blocks TOTP code replay
  settings       TEXT NOT NULL DEFAULT '{}',          -- JSON: theme, accent, sidebar, sort, descriptions
  last_login_at  INTEGER,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

CREATE TABLE webauthn_credentials (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  credential_id BLOB NOT NULL UNIQUE,
  credential    TEXT NOT NULL,                        -- the full go-webauthn credential, JSON
  name          TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER
);
CREATE INDEX webauthn_credentials_user ON webauthn_credentials(user_id);

CREATE TABLE recovery_codes (
  id        INTEGER PRIMARY KEY,
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL,
  used_at   INTEGER
);
CREATE INDEX recovery_codes_user ON recovery_codes(user_id);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  token_hash   BLOB NOT NULL UNIQUE,                  -- sha256 of the token
  scopes       TEXT NOT NULL DEFAULT 'read,write',
  last_used_at INTEGER,
  expires_at   INTEGER,
  created_at   INTEGER NOT NULL
);
CREATE INDEX api_tokens_user ON api_tokens(user_id);

CREATE TABLE invites (
  id         INTEGER PRIMARY KEY,
  token_hash BLOB NOT NULL UNIQUE,                    -- sha256 of the link token
  note       TEXT NOT NULL DEFAULT '',
  created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  used_by    INTEGER REFERENCES users(id) ON DELETE SET NULL
);

-- Instance-wide settings an admin can change (signup mode, 2FA policy,
-- who creates spaces) and bookkeeping such as the icon backfill marker.
CREATE TABLE instance_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

-- Spaces ----------------------------------------------------------------

-- Bookmarks, tags and saved searches belong to a space. Every user has a personal
-- space; shared spaces have several members.
CREATE TABLE spaces (
  id          INTEGER PRIMARY KEY,
  personal_of INTEGER UNIQUE REFERENCES users(id) ON DELETE CASCADE, -- NULL = a shared space
  name        TEXT NOT NULL DEFAULT '',                              -- shared spaces only
  created_at  INTEGER NOT NULL
);

CREATE TABLE space_members (
  space_id   INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('owner', 'write', 'read')),
  created_at INTEGER NOT NULL,
  PRIMARY KEY (space_id, user_id)
) WITHOUT ROWID;
CREATE INDEX space_members_user ON space_members(user_id);
CREATE UNIQUE INDEX space_members_one_owner ON space_members(space_id) WHERE role = 'owner';

-- Every new user gets a personal space, however the account is created.
-- +goose StatementBegin
CREATE TRIGGER users_personal_space AFTER INSERT ON users BEGIN
  INSERT INTO spaces (personal_of, created_at) VALUES (NEW.id, NEW.created_at);
  INSERT INTO space_members (space_id, user_id, role, created_at)
    SELECT id, NEW.id, 'owner', NEW.created_at FROM spaces WHERE personal_of = NEW.id;
END;
-- +goose StatementEnd

-- The space a browser session is looking at; NULL = the personal space.
CREATE TABLE sessions (
  token_hash   BLOB PRIMARY KEY,                      -- sha256 of the cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  mfa_passed   INTEGER NOT NULL DEFAULT 0,
  user_agent   TEXT,
  ip           TEXT,
  space_id     INTEGER REFERENCES spaces(id) ON DELETE SET NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  verified_at  INTEGER NOT NULL DEFAULT 0,            -- last password or second-factor check
  sso          INTEGER NOT NULL DEFAULT 0,            -- started through the provider, which does its own second factor
  sso_tokens   BLOB,                                  -- sso: the provider's access and refresh tokens, sealed
  groups_checked_at INTEGER NOT NULL DEFAULT 0          -- sso: when the provider last confirmed the user's groups
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expires ON sessions(expires_at);

-- An identity at the OpenID Connect provider, (issuer, subject), signs in as
-- one user. A user has at most one. Never matched by email or name, only by
-- subject.
CREATE TABLE oidc_identities (
  issuer       TEXT NOT NULL,
  subject      TEXT NOT NULL,
  user_id      INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  PRIMARY KEY (issuer, subject)
) WITHOUT ROWID;

-- Bookmarks -------------------------------------------------------------

CREATE TABLE favicons (                               -- shared by every space, one per host
  id         INTEGER PRIMARY KEY,
  domain     TEXT NOT NULL UNIQUE,
  mime       TEXT NOT NULL,
  data       BLOB NOT NULL,
  fetched_at INTEGER NOT NULL
);

-- created_by is who saved the bookmark; in a personal space, its owner.
CREATE TABLE bookmarks (
  id           INTEGER PRIMARY KEY,
  space_id     INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
  url          TEXT NOT NULL,
  url_hash     BLOB NOT NULL,                         -- sha256 of the normalised URL
  domain       TEXT NOT NULL DEFAULT '',              -- lower-cased host, for site: filters
  title        TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  notes        TEXT NOT NULL DEFAULT '',
  favicon_id   INTEGER REFERENCES favicons(id),
  is_archived  INTEGER NOT NULL DEFAULT 0,
  is_favourite INTEGER NOT NULL DEFAULT 0,
  share_token  TEXT UNIQUE,                           -- NULL = private
  fetch_status TEXT NOT NULL DEFAULT 'pending',       -- pending | done | failed: the page-details queue
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  UNIQUE (space_id, url_hash)
);
CREATE INDEX bookmarks_space_created ON bookmarks(space_id, created_at DESC);
CREATE INDEX bookmarks_space_domain ON bookmarks(space_id, domain);
CREATE INDEX bookmarks_fetch_pending ON bookmarks(id) WHERE fetch_status = 'pending';

CREATE TABLE tags (
  id       INTEGER PRIMARY KEY,
  space_id INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  name     TEXT NOT NULL COLLATE NOCASE,
  UNIQUE (space_id, name)
);

CREATE TABLE bookmark_tags (
  bookmark_id INTEGER NOT NULL REFERENCES bookmarks(id) ON DELETE CASCADE,
  tag_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (bookmark_id, tag_id)
) WITHOUT ROWID;
CREATE INDEX bookmark_tags_tag ON bookmark_tags(tag_id);

-- One person's saved search within one space.
CREATE TABLE saved_searches (
  id       INTEGER PRIMARY KEY,
  space_id INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name     TEXT NOT NULL,
  query    TEXT NOT NULL
);
CREATE INDEX saved_searches_space_user ON saved_searches(space_id, user_id);

-- Background jobs (imports), into the space they were started in.
CREATE TABLE jobs (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE,
  space_id    INTEGER REFERENCES spaces(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,                          -- import
  payload     TEXT NOT NULL,                          -- the uploaded file, dropped when done
  status      TEXT NOT NULL DEFAULT 'queued',         -- queued | running | done | failed
  attempts    INTEGER NOT NULL DEFAULT 0,
  run_after   INTEGER NOT NULL,
  progress    INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0,
  result      TEXT NOT NULL DEFAULT '',               -- JSON summary when done
  last_error  TEXT,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER
);
CREATE INDEX jobs_queue ON jobs(status, run_after);
CREATE INDEX jobs_user ON jobs(user_id, id DESC);

-- Full-text search ------------------------------------------------------

-- Contentless (the text lives in bookmarks), with contentless_delete so rows
-- can be removed by rowid. Kept in sync by the triggers below; rowid =
-- bookmarks.id.
CREATE VIRTUAL TABLE bookmarks_fts USING fts5(
  title, description, notes, url, tags,
  content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2'
);

-- +goose StatementBegin
CREATE TRIGGER bookmarks_fts_insert AFTER INSERT ON bookmarks BEGIN
  INSERT INTO bookmarks_fts(rowid, title, description, notes, url, tags)
  VALUES (NEW.id, NEW.title, NEW.description, NEW.notes, NEW.url, '');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER bookmarks_fts_update AFTER UPDATE OF title, description, notes, url ON bookmarks BEGIN
  DELETE FROM bookmarks_fts WHERE rowid = OLD.id;
  INSERT INTO bookmarks_fts(rowid, title, description, notes, url, tags)
  SELECT b.id, b.title, b.description, b.notes, b.url,
         COALESCE((SELECT group_concat(t.name, ' ') FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
                   WHERE bt.bookmark_id = b.id), '')
  FROM bookmarks b WHERE b.id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER bookmarks_fts_delete AFTER DELETE ON bookmarks BEGIN
  DELETE FROM bookmarks_fts WHERE rowid = OLD.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER bookmark_tags_fts_insert AFTER INSERT ON bookmark_tags BEGIN
  DELETE FROM bookmarks_fts WHERE rowid = NEW.bookmark_id;
  INSERT INTO bookmarks_fts(rowid, title, description, notes, url, tags)
  SELECT b.id, b.title, b.description, b.notes, b.url,
         COALESCE((SELECT group_concat(t.name, ' ') FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
                   WHERE bt.bookmark_id = b.id), '')
  FROM bookmarks b WHERE b.id = NEW.bookmark_id;
END;
-- +goose StatementEnd

-- When a bookmark is deleted its tag links cascade after the bookmark row is
-- gone, so the SELECT below finds nothing and no stale row is re-inserted.
-- +goose StatementBegin
CREATE TRIGGER bookmark_tags_fts_delete AFTER DELETE ON bookmark_tags BEGIN
  DELETE FROM bookmarks_fts WHERE rowid = OLD.bookmark_id;
  INSERT INTO bookmarks_fts(rowid, title, description, notes, url, tags)
  SELECT b.id, b.title, b.description, b.notes, b.url,
         COALESCE((SELECT group_concat(t.name, ' ') FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
                   WHERE bt.bookmark_id = b.id), '')
  FROM bookmarks b WHERE b.id = OLD.bookmark_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER tags_fts_rename AFTER UPDATE OF name ON tags BEGIN
  DELETE FROM bookmarks_fts WHERE rowid IN (SELECT bookmark_id FROM bookmark_tags WHERE tag_id = NEW.id);
  INSERT INTO bookmarks_fts(rowid, title, description, notes, url, tags)
  SELECT b.id, b.title, b.description, b.notes, b.url,
         COALESCE((SELECT group_concat(t.name, ' ') FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
                   WHERE bt.bookmark_id = b.id), '')
  FROM bookmarks b WHERE b.id IN (SELECT bookmark_id FROM bookmark_tags WHERE tag_id = NEW.id);
END;
-- +goose StatementEnd

-- +goose Down
DROP TABLE bookmarks_fts;
DROP TABLE jobs;
DROP TABLE saved_searches;
DROP TABLE bookmark_tags;
DROP TABLE tags;
DROP TABLE bookmarks;
DROP TABLE favicons;
DROP TABLE oidc_identities;
DROP TABLE sessions;
DROP TABLE space_members;
DROP TABLE spaces;
DROP TABLE instance_settings;
DROP TABLE invites;
DROP TABLE api_tokens;
DROP TABLE recovery_codes;
DROP TABLE webauthn_credentials;
DROP TABLE users;
