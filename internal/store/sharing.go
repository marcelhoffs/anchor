package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
)

// SetShared turns a bookmark's public link on or off. Turning it on keeps an
// existing link; turning it off and on again makes a new one.
func (s *Store) SetShared(ctx context.Context, sc Scope, id int64, on bool) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	var token any // NULL = private
	if on {
		b := make([]byte, 16)
		rand.Read(b)
		token = base64.RawURLEncoding.EncodeToString(b)
	}
	res, err := s.w.ExecContext(ctx,
		`UPDATE bookmarks SET share_token = CASE WHEN ? IS NULL THEN NULL ELSE coalesce(share_token, ?) END
		 WHERE space_id = ? AND id = ?`, token, token, sc.spaceID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SharedBookmark returns the bookmark behind a public link. A personal
// bookmark's link works while its owner's account is active; a shared
// space's, while the space exists.
func (s *Store) SharedBookmark(ctx context.Context, token string) (Bookmark, error) {
	if token == "" {
		return Bookmark{}, ErrNotFound
	}
	return scanBookmark(s.r.QueryRowContext(ctx,
		"SELECT "+bookmarkCols+` FROM bookmarks b JOIN spaces sp ON sp.id = b.space_id
		 LEFT JOIN users u ON u.id = sp.personal_of
		 WHERE b.share_token = ? AND (sp.personal_of IS NULL OR u.is_active = 1)`, token))
}
