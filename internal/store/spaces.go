package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
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

// Scope is one user's access to one space: the bookmarks, tags and saved searches
// it holds. Its fields are unexported, so the only way to get one is from
// this package, after checking the membership; the zero Scope matches
// nothing. Methods that read take a Scope, and those that write also check
// CanWrite.
type Scope struct {
	spaceID, userID int64
	role            Role
	personal        bool
}

func (sc Scope) SpaceID() int64   { return sc.spaceID }
func (sc Scope) UserID() int64    { return sc.userID }
func (sc Scope) Role() Role       { return sc.role }
func (sc Scope) IsPersonal() bool { return sc.personal }

func (sc Scope) CanWrite() bool { return sc.role == RoleWrite || sc.role == RoleOwner }

func (sc Scope) mustWrite() error {
	if !sc.CanWrite() {
		return ErrForbidden
	}
	return nil
}

// mustOwnShared is the check for managing a shared space: only its owner
// may, and a personal space has nothing to manage.
func (sc Scope) mustOwnShared() error {
	switch {
	case sc.spaceID == 0 || sc.personal:
		return ErrNotFound
	case sc.role != RoleOwner:
		return ErrForbidden
	}
	return nil
}

const scopeCols = "m.space_id, m.role, sp.personal_of IS NOT NULL"

func scanScope(userID int64, row *sql.Row) (Scope, error) {
	sc := Scope{userID: userID}
	if err := row.Scan(&sc.spaceID, &sc.role, &sc.personal); err != nil {
		return Scope{}, notFound(err)
	}
	return sc, nil
}

// Access returns userID's scope in a space; ErrNotFound if they aren't a
// member.
func (s *Store) Access(ctx context.Context, userID, spaceID int64) (Scope, error) {
	return scanScope(userID, s.r.QueryRowContext(ctx,
		"SELECT "+scopeCols+` FROM space_members m JOIN spaces sp ON sp.id = m.space_id
		 WHERE m.space_id = ? AND m.user_id = ?`, spaceID, userID))
}

// PersonalScope returns the user's scope in their own space.
func (s *Store) PersonalScope(ctx context.Context, userID int64) (Scope, error) {
	return scanScope(userID, s.r.QueryRowContext(ctx,
		"SELECT "+scopeCols+` FROM spaces sp JOIN space_members m ON m.space_id = sp.id AND m.user_id = sp.personal_of
		 WHERE sp.personal_of = ?`, userID))
}

// BookmarkScope returns userID's scope in the space that holds a bookmark,
// whichever space the user is looking at; ErrNotFound if the bookmark
// doesn't exist or they aren't a member of its space.
func (s *Store) BookmarkScope(ctx context.Context, userID, bookmarkID int64) (Scope, error) {
	return scanScope(userID, s.r.QueryRowContext(ctx,
		"SELECT "+scopeCols+` FROM bookmarks b JOIN space_members m ON m.space_id = b.space_id
		 JOIN spaces sp ON sp.id = b.space_id WHERE b.id = ? AND m.user_id = ?`, bookmarkID, userID))
}

// Space is a shared space, as one member sees it.
type Space struct {
	ID        int64
	Name      string
	Icon      string // one of SpaceIcons, or "" for the default
	Role      Role   // the viewing member's
	Owner     string // the owner's username
	Members   int
	Bookmarks int
	CreatedAt time.Time
}

const spaceCols = `sp.id, sp.name, sp.icon, m.role,
	coalesce((SELECT u.username FROM space_members o JOIN users u ON u.id = o.user_id
	          WHERE o.space_id = sp.id AND o.role = 'owner'), ''),
	(SELECT count(*) FROM space_members WHERE space_id = sp.id),
	(SELECT count(*) FROM bookmarks WHERE space_id = sp.id), sp.created_at`

func scanSpace(row interface{ Scan(...any) error }) (Space, error) {
	var sp Space
	var created int64
	err := row.Scan(&sp.ID, &sp.Name, &sp.Icon, &sp.Role, &sp.Owner, &sp.Members, &sp.Bookmarks, &created)
	sp.CreatedAt = fromUnix(created)
	return sp, notFound(err)
}

// ListSpaces returns the shared spaces userID belongs to, by name.
func (s *Store) ListSpaces(ctx context.Context, userID int64) ([]Space, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+spaceCols+` FROM spaces sp JOIN space_members m ON m.space_id = sp.id AND m.user_id = ?
		 WHERE sp.personal_of IS NULL ORDER BY sp.name COLLATE NOCASE, sp.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Space
	for rows.Next() {
		sp, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// GetSpace returns the shared space sc points at; ErrNotFound for a
// personal one.
func (s *Store) GetSpace(ctx context.Context, sc Scope) (Space, error) {
	if sc.personal {
		return Space{}, ErrNotFound
	}
	return scanSpace(s.r.QueryRowContext(ctx,
		"SELECT "+spaceCols+` FROM spaces sp JOIN space_members m ON m.space_id = sp.id AND m.user_id = ?
		 WHERE sp.id = ? AND sp.personal_of IS NULL`, sc.userID, sc.spaceID))
}

const maxSpaceName = 100

func cleanSpaceName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	switch {
	case name == "":
		return "", invalid("Give the space a name.")
	case utf8.RuneCountInString(name) > maxSpaceName:
		return "", invalid("Space names are at most %d characters.", maxSpaceName)
	}
	return name, nil
}

// CreateSpace makes a shared space owned by userID and returns their scope
// in it.
func (s *Store) CreateSpace(ctx context.Context, userID int64, name string) (Scope, error) {
	name, err := cleanSpaceName(name)
	if err != nil {
		return Scope{}, err
	}
	sc := Scope{userID: userID, role: RoleOwner}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := unix(s.now())
		if err := tx.QueryRowContext(ctx, "INSERT INTO spaces (name, created_at) VALUES (?, ?) RETURNING id",
			name, now).Scan(&sc.spaceID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO space_members (space_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)",
			sc.spaceID, userID, now)
		return err
	})
	if err != nil {
		return Scope{}, err
	}
	return sc, nil
}

// RenameSpace changes a shared space's name; owner only.
func (s *Store) RenameSpace(ctx context.Context, sc Scope, name string) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	name, err := cleanSpaceName(name)
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, "UPDATE spaces SET name = ? WHERE id = ?", name, sc.spaceID)
	return err
}

// SpaceIcons are the icons a shared space can have, besides the default
// (symbols of the same name in static/icons.svg).
var SpaceIcons = []string{
	"briefcase", "house", "users", "folder", "book-open", "graduation-cap", "code", "server", "cpu", "globe",
	"heart", "heart-pulse", "stethoscope", "hospital", "ambulance", "siren", "rocket", "shopping-cart", "music",
	"camera", "gamepad-2", "wrench", "flask-conical", "leaf", "building-2", "trophy", "volleyball", "sport-shoe",
	"dumbbell", "bike", "car", "bus", "truck", "train-front", "plane", "plane-takeoff", "tower-control", "sailboat", "ship",
}

// SetSpaceIcon gives a shared space one of SpaceIcons, or the default for
// ""; owner only.
func (s *Store) SetSpaceIcon(ctx context.Context, sc Scope, icon string) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	if icon != "" && !slices.Contains(SpaceIcons, icon) {
		return invalid("Pick one of the space icons.")
	}
	_, err := s.w.ExecContext(ctx, "UPDATE spaces SET icon = ? WHERE id = ?", icon, sc.spaceID)
	return err
}

// DeleteSpace removes a shared space with its bookmarks, tags, saved searches and
// memberships; owner only.
func (s *Store) DeleteSpace(ctx context.Context, sc Scope) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	_, err := s.w.ExecContext(ctx, "DELETE FROM spaces WHERE id = ? AND personal_of IS NULL", sc.spaceID)
	return err
}

// Member is one person in a shared space.
type Member struct {
	UserID   int64
	Username string
	Role     Role
	IsActive bool
	AddedAt  time.Time
}

// Members lists a shared space's members, owner first; any member may see
// them.
func (s *Store) Members(ctx context.Context, sc Scope) ([]Member, error) {
	if sc.spaceID == 0 || sc.personal {
		return nil, ErrNotFound
	}
	return s.members(ctx, sc.spaceID)
}

func (s *Store) members(ctx context.Context, spaceID int64) ([]Member, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT u.id, u.username, m.role, u.is_active, m.created_at FROM space_members m JOIN users u ON u.id = m.user_id
		 WHERE m.space_id = ? ORDER BY m.role != 'owner', u.username COLLATE NOCASE`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var added int64
		if err := rows.Scan(&m.UserID, &m.Username, &m.Role, &m.IsActive, &added); err != nil {
			return nil, err
		}
		m.AddedAt = fromUnix(added)
		out = append(out, m)
	}
	return out, rows.Err()
}

func memberRole(role Role) error {
	if role != RoleWrite && role != RoleRead {
		return invalid("Choose whether the member can edit or only read.")
	}
	return nil
}

// AddMember adds an existing user to a shared space; owner only.
func (s *Store) AddMember(ctx context.Context, sc Scope, username string, role Role) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	if err := memberRole(role); err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	var userID int64
	err := s.r.QueryRowContext(ctx, "SELECT id FROM users WHERE username = ? AND is_active = 1", username).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("There's no user called %q.", username)
	}
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, "INSERT INTO space_members (space_id, user_id, role, created_at) VALUES (?, ?, ?, ?)",
		sc.spaceID, userID, role, unix(s.now()))
	if isUniqueViolation(err) {
		return invalid("%s is already a member.", username)
	}
	return err
}

// SuggestMembers returns up to 8 active usernames containing q that aren't
// in the space yet, those starting with it first; for the owner's
// add-member field.
func (s *Store) SuggestMembers(ctx context.Context, sc Scope, q string) ([]string, error) {
	if err := sc.mustOwnShared(); err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT username FROM users u
		 WHERE is_active = 1 AND instr(lower(username), lower(?)) > 0
		   AND NOT EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = ? AND m.user_id = u.id)
		 ORDER BY instr(lower(username), lower(?)) != 1, username COLLATE NOCASE LIMIT 8`, q, sc.spaceID, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// SetRole changes a member's role between write and read; owner only. The
// owner's own role changes only by handing the space over.
func (s *Store) SetRole(ctx context.Context, sc Scope, userID int64, role Role) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	if err := memberRole(role); err != nil {
		return err
	}
	return affected(s.w.ExecContext(ctx, "UPDATE space_members SET role = ? WHERE space_id = ? AND user_id = ? AND role != 'owner'",
		role, sc.spaceID, userID))
}

// RemoveMember takes someone out of a shared space; owner only.
func (s *Store) RemoveMember(ctx context.Context, sc Scope, userID int64) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	return s.dropMember(ctx, sc.spaceID, userID)
}

// LeaveSpace takes the scope's user out of a shared space. The owner can't
// leave: they hand the space over or delete it.
func (s *Store) LeaveSpace(ctx context.Context, sc Scope) error {
	switch {
	case sc.spaceID == 0 || sc.personal:
		return ErrNotFound
	case sc.role == RoleOwner:
		return invalid("Make another member the owner before you leave, or delete the space.")
	}
	return s.dropMember(ctx, sc.spaceID, sc.userID)
}

// dropMember takes a non-owner out of a space, with their saved searches
// in it: they would otherwise come back if the user were added again.
func (s *Store) dropMember(ctx context.Context, spaceID, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		err := affected(tx.ExecContext(ctx, "DELETE FROM space_members WHERE space_id = ? AND user_id = ? AND role != 'owner'",
			spaceID, userID))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM saved_searches WHERE space_id = ? AND user_id = ?", spaceID, userID)
		return err
	})
}

// TransferSpace makes another member the owner; the old owner stays on as
// a member who can edit. Owner only.
func (s *Store) TransferSpace(ctx context.Context, sc Scope, userID int64) error {
	if err := sc.mustOwnShared(); err != nil {
		return err
	}
	if userID == sc.userID {
		return invalid("You already own this space.")
	}
	return s.transfer(ctx, sc.spaceID, userID)
}

// transfer makes member userID the owner of a shared space; the old owner
// stays on as an editor. ErrNotFound, and nothing changed, if userID isn't
// a member.
func (s *Store) transfer(ctx context.Context, spaceID, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		// One owner per space: step down first, then promote.
		if _, err := tx.ExecContext(ctx, "UPDATE space_members SET role = 'write' WHERE space_id = ? AND role = 'owner'",
			spaceID); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx,
			`UPDATE space_members SET role = 'owner' WHERE space_id = ? AND user_id = ?
			 AND space_id IN (SELECT id FROM spaces WHERE personal_of IS NULL)`, spaceID, userID))
	})
}

// The Admin methods below manage shared spaces as a whole, for instance one
// whose owner was disabled. They never read or return the bookmarks inside,
// and are only reachable from the admin pages.

// AdminSpace is a shared space as admins see it: who's in it and how big
// it is, not what it holds.
type AdminSpace struct {
	ID          int64
	Name        string
	Icon        string
	Bookmarks   int
	CreatedAt   time.Time
	Members     []Member // owner first
	OwnerActive bool
}

// AdminListSpaces returns every shared space with its members, by name.
func (s *Store) AdminListSpaces(ctx context.Context) ([]AdminSpace, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT sp.id, sp.name, sp.icon, sp.created_at, (SELECT count(*) FROM bookmarks WHERE space_id = sp.id)
		 FROM spaces sp WHERE sp.personal_of IS NULL ORDER BY sp.name COLLATE NOCASE, sp.id`)
	if err != nil {
		return nil, err
	}
	var out []AdminSpace
	for rows.Next() {
		var sp AdminSpace
		var created int64
		if err := rows.Scan(&sp.ID, &sp.Name, &sp.Icon, &created, &sp.Bookmarks); err != nil {
			rows.Close()
			return nil, err
		}
		sp.CreatedAt = fromUnix(created)
		out = append(out, sp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Members, err = s.members(ctx, out[i].ID); err != nil {
			return nil, err
		}
		for _, m := range out[i].Members {
			if m.Role == RoleOwner {
				out[i].OwnerActive = m.IsActive
			}
		}
	}
	return out, nil
}

// AdminTransferSpace makes a member the owner of a shared space.
func (s *Store) AdminTransferSpace(ctx context.Context, spaceID, userID int64) error {
	return s.transfer(ctx, spaceID, userID)
}

// AdminDeleteSpace deletes a shared space and everything in it.
func (s *Store) AdminDeleteSpace(ctx context.Context, spaceID int64) error {
	return affected(s.w.ExecContext(ctx, "DELETE FROM spaces WHERE id = ? AND personal_of IS NULL", spaceID))
}

// affected turns an UPDATE or DELETE that matched no row into ErrNotFound.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
