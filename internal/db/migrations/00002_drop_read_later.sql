-- Read later was removed as a feature.

-- +goose Up
ALTER TABLE bookmarks DROP COLUMN is_unread;

-- +goose Down
ALTER TABLE bookmarks ADD COLUMN is_unread INTEGER NOT NULL DEFAULT 0;
