-- A colour per tag, one of store.TagColors. Empty picks one from the name.

-- +goose Up
ALTER TABLE tags ADD COLUMN color TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tags DROP COLUMN color;
