-- The page-details fetcher looks for pending bookmarks after every batch;
-- this keeps that from scanning the whole table.

-- +goose Up
CREATE INDEX bookmarks_fetch_pending ON bookmarks(id) WHERE fetch_status = 'pending';

-- +goose Down
DROP INDEX bookmarks_fetch_pending;
