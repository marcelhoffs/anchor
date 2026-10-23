-- Pages that redirect to another host of the same site (Google services to
-- the Google sign-in page) still gave bookmarks that page's generic icon.
-- Fetch every icon again with the stricter rule.

-- +goose Up
UPDATE bookmarks SET favicon_id = NULL, fetch_status = 'pending';
DELETE FROM favicons;

-- +goose Down
SELECT 1; -- the old icons are gone; they are fetched again on their own
