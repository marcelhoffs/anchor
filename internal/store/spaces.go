package store

import (
	"context"
	"errors"
)

// ErrForbidden means the user may see the space but not change it.
var ErrForbidden = errors.New("forbidden")

// Role is what a member may do in a space.
type Role string

const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleOwner Role = "owner" // write, and manage the space
)

// Scope is one user's access to one space: the bookmarks, tags and bundles
// it holds. Its fields are unexported, so the only way to get one is from
// this package, after checking the membership; the zero Scope matches
// nothing. Methods that read take a Scope, and those that write also check
// CanWrite.
type Scope struct {
	spaceID, userID int64
	role            Role
}

func (sc Scope) SpaceID() int64 { return sc.spaceID }
func (sc Scope) UserID() int64  { return sc.userID }
func (sc Scope) Role() Role     { return sc.role }

func (sc Scope) CanWrite() bool { return sc.role == RoleWrite || sc.role == RoleOwner }

func (sc Scope) mustWrite() error {
	if !sc.CanWrite() {
		return ErrForbidden
	}
	return nil
}

// Access returns userID's scope in a space; ErrNotFound if they aren't a
// member.
func (s *Store) Access(ctx context.Context, userID, spaceID int64) (Scope, error) {
	sc := Scope{spaceID: spaceID, userID: userID}
	err := s.r.QueryRowContext(ctx, "SELECT role FROM space_members WHERE space_id = ? AND user_id = ?",
		spaceID, userID).Scan(&sc.role)
	if err != nil {
		return Scope{}, notFound(err)
	}
	return sc, nil
}

// PersonalScope returns the user's scope in their own space.
func (s *Store) PersonalScope(ctx context.Context, userID int64) (Scope, error) {
	sc := Scope{userID: userID}
	err := s.r.QueryRowContext(ctx,
		`SELECT m.space_id, m.role FROM spaces sp JOIN space_members m ON m.space_id = sp.id AND m.user_id = sp.personal_of
		 WHERE sp.personal_of = ?`, userID).Scan(&sc.spaceID, &sc.role)
	if err != nil {
		return Scope{}, notFound(err)
	}
	return sc, nil
}

// BookmarkScope returns userID's scope in the space that holds a bookmark,
// whichever space the user is looking at; ErrNotFound if the bookmark
// doesn't exist or they aren't a member of its space.
func (s *Store) BookmarkScope(ctx context.Context, userID, bookmarkID int64) (Scope, error) {
	sc := Scope{userID: userID}
	err := s.r.QueryRowContext(ctx,
		`SELECT m.space_id, m.role FROM bookmarks b JOIN space_members m ON m.space_id = b.space_id
		 WHERE b.id = ? AND m.user_id = ?`, bookmarkID, userID).Scan(&sc.spaceID, &sc.role)
	if err != nil {
		return Scope{}, notFound(err)
	}
	return sc, nil
}
