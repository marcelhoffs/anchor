-- +goose Up
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email         TEXT UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,              -- argon2id encoded
  is_admin      INTEGER NOT NULL DEFAULT 0,
  is_active     INTEGER NOT NULL DEFAULT 1,
  totp_secret   BLOB,                       -- encrypted (AES-GCM, app key)
  totp_enabled  INTEGER NOT NULL DEFAULT 0,
  settings      TEXT NOT NULL DEFAULT '{}', -- JSON: theme, page size, defaults
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE webauthn_credentials (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  credential_id BLOB NOT NULL UNIQUE,
  public_key    BLOB NOT NULL,
  sign_count    INTEGER NOT NULL DEFAULT 0,
  name          TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER
);

CREATE TABLE recovery_codes (
  id        INTEGER PRIMARY KEY,
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL,
  used_at   INTEGER
);

CREATE TABLE sessions (
  token_hash   BLOB PRIMARY KEY,           -- sha256 of cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  mfa_passed   INTEGER NOT NULL DEFAULT 0,
  user_agent   TEXT,
  ip           TEXT,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  token_hash   BLOB NOT NULL UNIQUE,
  scopes       TEXT NOT NULL DEFAULT 'read,write',
  last_used_at INTEGER,
  expires_at   INTEGER,
  created_at   INTEGER NOT NULL
);

CREATE TABLE favicons (                    -- shared, deduped by domain
  id         INTEGER PRIMARY KEY,
  domain     TEXT NOT NULL UNIQUE,
  mime       TEXT NOT NULL,
  data       BLOB NOT NULL,
  fetched_at INTEGER NOT NULL
);

CREATE TABLE bookmarks (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  url          TEXT NOT NULL,
  url_hash     BLOB NOT NULL,               -- sha256 of normalised URL
  domain       TEXT NOT NULL DEFAULT '',    -- lower-cased host, for site: filters
  title        TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  notes        TEXT NOT NULL DEFAULT '',
  favicon_id   INTEGER REFERENCES favicons(id),
  preview_url  TEXT,
  is_unread    INTEGER NOT NULL DEFAULT 0,
  is_archived  INTEGER NOT NULL DEFAULT 0,
  is_favourite INTEGER NOT NULL DEFAULT 0,
  share_token  TEXT UNIQUE,                 -- NULL = private
  fetch_status TEXT NOT NULL DEFAULT 'pending',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  UNIQUE (user_id, url_hash)
);
CREATE INDEX bookmarks_user_created ON bookmarks(user_id, created_at DESC);
CREATE INDEX bookmarks_user_domain ON bookmarks(user_id, domain);

CREATE TABLE tags (
  id      INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name    TEXT NOT NULL COLLATE NOCASE,
  UNIQUE (user_id, name)
);

CREATE TABLE bookmark_tags (
  bookmark_id INTEGER NOT NULL REFERENCES bookmarks(id) ON DELETE CASCADE,
  tag_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (bookmark_id, tag_id)
) WITHOUT ROWID;
CREATE INDEX bookmark_tags_tag ON bookmark_tags(tag_id);

CREATE TABLE bundles (                     -- saved searches
  id       INTEGER PRIMARY KEY,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name     TEXT NOT NULL,
  query    TEXT NOT NULL,
  position INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,                -- fetch_meta, import, check_link
  payload    TEXT NOT NULL,                -- JSON
  status     TEXT NOT NULL DEFAULT 'queued',
  attempts   INTEGER NOT NULL DEFAULT 0,
  run_after  INTEGER NOT NULL,
  last_error TEXT
);

CREATE TABLE audit_log (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER,
  event      TEXT NOT NULL,                -- login_ok, login_fail, mfa_enabled…
  ip         TEXT,
  created_at INTEGER NOT NULL
);

-- Full-text search. Contentless (the text lives in bookmarks), with
-- contentless_delete so rows can be removed by rowid. Kept in sync by the
-- triggers below; rowid = bookmarks.id.
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
DROP TABLE audit_log;
DROP TABLE jobs;
DROP TABLE bundles;
DROP TABLE bookmark_tags;
DROP TABLE tags;
DROP TABLE bookmarks;
DROP TABLE favicons;
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE recovery_codes;
DROP TABLE webauthn_credentials;
DROP TABLE users;
