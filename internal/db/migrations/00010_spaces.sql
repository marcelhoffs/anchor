-- Spaces: bookmarks, tags and bundles now belong to a space instead of a
-- user. Every user has a personal space; shared spaces have several
-- members. space_members says who may do what in each.
--
-- Bookmarks, tags, their links, bundles and the search index are recreated
-- empty rather than converted: Anchor is still in development, and the
-- bookmarks are imported again from an export.

-- +goose Up
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

INSERT INTO spaces (personal_of, created_at) SELECT id, created_at FROM users;
INSERT INTO space_members (space_id, user_id, role, created_at)
  SELECT id, personal_of, 'owner', created_at FROM spaces;

-- Every new user gets a personal space, however the account is created.
-- +goose StatementBegin
CREATE TRIGGER users_personal_space AFTER INSERT ON users BEGIN
  INSERT INTO spaces (personal_of, created_at) VALUES (NEW.id, NEW.created_at);
  INSERT INTO space_members (space_id, user_id, role, created_at)
    SELECT id, NEW.id, 'owner', NEW.created_at FROM spaces WHERE personal_of = NEW.id;
END;
-- +goose StatementEnd

-- Dropping a table drops its triggers too.
DROP TABLE bookmarks_fts;
DROP TABLE bookmark_tags;
DROP TABLE bundles;
DROP TABLE tags;
DROP TABLE bookmarks;
DELETE FROM jobs;

-- created_by is who saved the bookmark; in a personal space, its owner.
CREATE TABLE bookmarks (
  id           INTEGER PRIMARY KEY,
  space_id     INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
  url          TEXT NOT NULL,
  url_hash     BLOB NOT NULL,               -- sha256 of normalised URL
  domain       TEXT NOT NULL DEFAULT '',    -- lower-cased host, for site: filters
  title        TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  notes        TEXT NOT NULL DEFAULT '',
  favicon_id   INTEGER REFERENCES favicons(id),
  is_archived  INTEGER NOT NULL DEFAULT 0,
  is_favourite INTEGER NOT NULL DEFAULT 0,
  share_token  TEXT UNIQUE,                 -- NULL = private
  fetch_status TEXT NOT NULL DEFAULT 'pending',
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

-- A bundle is one person's saved search within one space.
CREATE TABLE bundles (
  id       INTEGER PRIMARY KEY,
  space_id INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name     TEXT NOT NULL,
  query    TEXT NOT NULL,
  position INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX bundles_space_user ON bundles(space_id, user_id);

-- An import goes into the space it was started in.
ALTER TABLE jobs ADD COLUMN space_id INTEGER REFERENCES spaces(id) ON DELETE CASCADE;

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
-- The old per-user tables can't be brought back; restore a backup taken
-- before this migration instead. This statement fails on purpose.
SELECT * FROM "migration 00010 (spaces) can't be undone: restore a backup";
