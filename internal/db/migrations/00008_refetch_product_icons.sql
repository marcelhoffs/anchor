-- Icon services answer for product hosts (keep.google.com) with the parent's
-- brand icon, in a form that differs byte for byte from the parent's own.
-- Such answers are no longer used; fetch every icon again without them.

-- +goose Up
UPDATE bookmarks SET favicon_id = NULL, fetch_status = 'pending';
DELETE FROM favicons;

-- +goose Down
SELECT 1; -- the old icons are gone; they are fetched again on their own
