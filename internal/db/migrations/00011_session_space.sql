-- The space a browser session is looking at; NULL = the personal space.
-- Leaving or losing a space sends the session back to the personal one.

-- +goose Up
ALTER TABLE sessions ADD COLUMN space_id INTEGER REFERENCES spaces(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN space_id;
