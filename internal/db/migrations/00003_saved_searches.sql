-- Bundles are now called saved searches.

-- +goose Up
ALTER TABLE bundles RENAME TO saved_searches;
DROP INDEX bundles_space_user;
CREATE INDEX saved_searches_space_user ON saved_searches(space_id, user_id);

-- +goose Down
DROP INDEX saved_searches_space_user;
ALTER TABLE saved_searches RENAME TO bundles;
CREATE INDEX bundles_space_user ON bundles(space_id, user_id);
