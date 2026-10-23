-- Some sites' favicon.ico has no visible pixel (online.beobank.be), and a
-- host that redirects to its site's home page now takes that page's icon.
-- Fetch every icon again with both rules.

-- +goose Up
UPDATE bookmarks SET favicon_id = NULL, fetch_status = 'pending';
DELETE FROM favicons;

-- +goose Down
SELECT 1; -- the old icons are gone; they are fetched again on their own
