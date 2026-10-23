-- Icons fetched before now could belong to a cookie-consent site that a page
-- redirected to (DPG Media's privacy gate, for one). Drop every stored icon
-- and fetch them again; fetching only fills empty titles and descriptions,
-- so nothing the user entered changes.

-- +goose Up
UPDATE bookmarks SET favicon_id = NULL, fetch_status = 'pending';
DELETE FROM favicons;

-- +goose Down
SELECT 1; -- the old icons are gone; they are fetched again on their own
