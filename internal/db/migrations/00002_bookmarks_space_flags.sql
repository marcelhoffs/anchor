-- The list's match count and the sidebar's view counts filter a space's
-- bookmarks by these flags; this index answers both without reading rows.

-- +goose Up
CREATE INDEX bookmarks_space_flags ON bookmarks(space_id, is_archived, is_favourite);

-- +goose Down
DROP INDEX bookmarks_space_flags;
