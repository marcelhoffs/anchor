package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
)

// SetShared turns a bookmark's public link on or off. Turning it on keeps an
// existing link; turning it off and on again makes a new one.
func (s *Store) SetShared(ctx context.Context, userID, id int64, on bool) error {
	var token any // NULL = private
	if on {
		b := make([]byte, 16)
		rand.Read(b)
		token = base64.RawURLEncoding.EncodeToString(b)
	}
	res, err := s.w.ExecContext(ctx,
		`UPDATE bookmarks SET share_token = CASE WHEN ? IS NULL THEN NULL ELSE coalesce(share_token, ?) END
		 WHERE user_id = ? AND id = ?`, token, token, userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SharedBookmark returns the bookmark behind a public link, as long as its
// owner's account is active.
func (s *Store) SharedBookmark(ctx context.Context, token string) (Bookmark, error) {
	if token == "" {
		return Bookmark{}, ErrNotFound
	}
	return scanBookmark(s.r.QueryRowContext(ctx,
		"SELECT "+bookmarkCols+` FROM bookmarks b JOIN users u ON u.id = b.user_id
		 WHERE b.share_token = ? AND u.is_active = 1`, token))
}
