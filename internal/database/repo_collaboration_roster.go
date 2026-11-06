package database

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// CollaborationRoster stores the collaborator roster of a repository. A
// repository has at most one roster, the raw content is kept so it can be
// reviewed and reapplied.
type CollaborationRoster struct {
	ID          int64  `xorm:"pk autoincr" gorm:"primaryKey"`
	RepoID      int64  `xorm:"UNIQUE NOT NULL" gorm:"uniqueIndex;not null"`
	Content     string `xorm:"TEXT NOT NULL" gorm:"not null;type:text"`
	UpdatedBy   int64
	UpdatedUnix int64
}

// CollaborationRosterEntry is a parsed roster line, the username in lower
// case paired with the desired access mode.
type CollaborationRosterEntry struct {
	Name string
	Mode AccessMode
}

// RosterValidationErrorKind identifies why a roster cannot be applied.
type RosterValidationErrorKind int

const (
	// RosterErrorInvalidEntry means a line does not contain exactly a
	// username and an access level.
	RosterErrorInvalidEntry RosterValidationErrorKind = iota + 1
	// RosterErrorInvalidMode means the access level is not read, write or
	// admin.
	RosterErrorInvalidMode
	// RosterErrorDuplicateUser means the same username appears on multiple
	// lines.
	RosterErrorDuplicateUser
	// RosterErrorUserNotExist means no user matches one of the usernames.
	RosterErrorUserNotExist
	// RosterErrorUserIsOrganization means an organization is listed, only
	// individual users can be collaborators.
	RosterErrorUserIsOrganization
	// RosterErrorUserIsOwner means the repository owner is listed, the owner
	// already holds owner access without a collaboration.
	RosterErrorUserIsOwner
)

// RosterValidationError describes the first problem found while validating a
// roster. No permission is changed when it is returned.
type RosterValidationError struct {
	Kind      RosterValidationErrorKind
	Line      int    // 1-based line number, zero when not tied to a line.
	FirstLine int    // 1-based line number of the first occurrence, for duplicates.
	Name      string // Related username in lower case.
	Token     string // Offending raw token.
}

func (err RosterValidationError) Error() string {
	switch err.Kind {
	case RosterErrorInvalidEntry:
		return fmt.Sprintf("invalid roster entry at line %d: %q", err.Line, err.Token)
	case RosterErrorInvalidMode:
		return fmt.Sprintf("invalid access mode %q at line %d", err.Token, err.Line)
	case RosterErrorDuplicateUser:
		return fmt.Sprintf("duplicate user %q at lines %d and %d", err.Name, err.FirstLine, err.Line)
	case RosterErrorUserNotExist:
		return fmt.Sprintf("user does not exist: %s", err.Name)
	case RosterErrorUserIsOrganization:
		return fmt.Sprintf("organization cannot be a collaborator: %s", err.Name)
	case RosterErrorUserIsOwner:
		return fmt.Sprintf("repository owner cannot be a collaborator: %s", err.Name)
	default:
		return "invalid collaborator roster"
	}
}

// IsRosterValidationError returns the validation error when the failure
// happened while validating the roster.
func IsRosterValidationError(err error) (*RosterValidationError, bool) {
	var verr RosterValidationError
	if errors.As(err, &verr) {
		return &verr, true
	}
	return nil, false
}

// ParseCollaborationRoster parses the raw roster content into entries. Each
// non-empty line must contain a username and one of read, write or admin
// separated by whitespace, e.g. "alice write". Lines starting with "#" are
// comments. It fails on the first malformed line or duplicate username.
func ParseCollaborationRoster(content string) ([]CollaborationRosterEntry, error) {
	entries := make([]CollaborationRosterEntry, 0)
	firstSeen := make(map[string]int)

	for lineNum, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, RosterValidationError{
				Kind:  RosterErrorInvalidEntry,
				Line:  lineNum + 1,
				Token: line,
			}
		}

		name := strings.ToLower(fields[0])
		mode := parseRosterAccessMode(fields[1])
		if mode == AccessModeNone {
			return nil, RosterValidationError{
				Kind:  RosterErrorInvalidMode,
				Line:  lineNum + 1,
				Name:  name,
				Token: fields[1],
			}
		}

		if firstLine, ok := firstSeen[name]; ok {
			return nil, RosterValidationError{
				Kind:      RosterErrorDuplicateUser,
				Line:      lineNum + 1,
				FirstLine: firstLine,
				Name:      name,
			}
		}
		firstSeen[name] = lineNum + 1

		entries = append(entries, CollaborationRosterEntry{
			Name: name,
			Mode: mode,
		})
	}
	return entries, nil
}

func parseRosterAccessMode(raw string) AccessMode {
	switch strings.ToLower(raw) {
	case "read":
		return AccessModeRead
	case "write":
		return AccessModeWrite
	case "admin":
		return AccessModeAdmin
	default:
		return AccessModeNone
	}
}

// CollaborationRosterResult summarizes a roster application. All slices
// contain usernames.
type CollaborationRosterResult struct {
	Added         []string
	ModeChanged   []string
	Removed       []string
	SkippedManual []string
}

// ApplyCollaborationRoster validates and applies the roster atomically.
//
// Roster entries that are not collaborators are added and marked as managed
// by the roster. Roster-managed collaborators are updated to the listed
// access mode, and removed when no longer listed. Collaborators added
// manually are never modified, even when their username happens to be in the
// roster.
//
// All usernames are resolved and the whole roster is validated before any
// write, and all writes happen in a single transaction, so invalid input or
// a mid-processing failure leaves all permissions untouched.
func (r *Repository) ApplyCollaborationRoster(actorID int64, content string) (*CollaborationRosterResult, error) {
	entries, err := ParseCollaborationRoster(content)
	if err != nil {
		return nil, err
	}

	// Resolve and validate every username up front, nothing must be written
	// when the roster cannot be fully applied.
	desiredModes := make(map[int64]AccessMode, len(entries))
	resolvedUsers := make([]*User, 0, len(entries))
	for _, entry := range entries {
		user := &User{LowerName: entry.Name}
		has, err := x.Get(user)
		if err != nil {
			return nil, errors.Newf("get user %q: %v", entry.Name, err)
		} else if !has {
			return nil, RosterValidationError{
				Kind: RosterErrorUserNotExist,
				Name: entry.Name,
			}
		}

		if user.ID == r.OwnerID {
			return nil, RosterValidationError{
				Kind: RosterErrorUserIsOwner,
				Name: entry.Name,
			}
		}
		if user.IsOrganization() {
			return nil, RosterValidationError{
				Kind: RosterErrorUserIsOrganization,
				Name: entry.Name,
			}
		}

		desiredModes[user.ID] = entry.Mode
		resolvedUsers = append(resolvedUsers, user)
	}

	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return nil, errors.Wrap(err, "begin transaction")
	}
	rollback := func(cause error) (*CollaborationRosterResult, error) {
		_ = sess.Rollback()
		return nil, cause
	}

	collaborations := make([]*Collaboration, 0)
	if err = sess.Find(&collaborations, &Collaboration{RepoID: r.ID}); err != nil {
		return rollback(errors.Newf("get collaborations [repo_id: %d]: %v", r.ID, err))
	}
	existing := make(map[int64]*Collaboration, len(collaborations))
	for _, collaboration := range collaborations {
		existing[collaboration.UserID] = collaboration
	}

	result := &CollaborationRosterResult{}
	userName := func(userID int64) (string, error) {
		user, err := getUserByID(sess, userID)
		if err != nil {
			return "", err
		}
		return user.Name, nil
	}

	// Reconcile current collaborators: remove stale roster-managed ones,
	// update changed modes, and leave manually added collaborators alone.
	for _, collaboration := range collaborations {
		mode, wanted := desiredModes[collaboration.UserID]
		switch {
		case collaboration.ManagedByRoster && !wanted:
			if _, err = sess.Delete(&Collaboration{
				RepoID: r.ID,
				UserID: collaboration.UserID,
			}); err != nil {
				return rollback(errors.Newf("delete collaboration [user_id: %d]: %v", collaboration.UserID, err))
			}
			name, err := userName(collaboration.UserID)
			if err != nil {
				return rollback(err)
			}
			result.Removed = append(result.Removed, name)

		case collaboration.ManagedByRoster && collaboration.Mode != mode:
			collaboration.Mode = mode
			if _, err = sess.ID(collaboration.ID).AllCols().Update(collaboration); err != nil {
				return rollback(errors.Newf("update collaboration [user_id: %d]: %v", collaboration.UserID, err))
			}
			name, err := userName(collaboration.UserID)
			if err != nil {
				return rollback(err)
			}
			result.ModeChanged = append(result.ModeChanged, name)

		case !collaboration.ManagedByRoster && wanted:
			name, err := userName(collaboration.UserID)
			if err != nil {
				return rollback(err)
			}
			result.SkippedManual = append(result.SkippedManual, name)
		}
	}

	// Add roster users that are not collaborators yet.
	for _, user := range resolvedUsers {
		if _, ok := existing[user.ID]; ok {
			continue
		}
		collaboration := &Collaboration{
			RepoID:          r.ID,
			UserID:          user.ID,
			Mode:            desiredModes[user.ID],
			ManagedByRoster: true,
		}
		if _, err = sess.Insert(collaboration); err != nil {
			return rollback(errors.Newf("insert collaboration [user_id: %d]: %v", user.ID, err))
		}
		result.Added = append(result.Added, user.Name)
	}

	if err = r.recalculateAccesses(sess); err != nil {
		return rollback(errors.Newf("recalculate accesses [repo_id: %d]: %v", r.ID, err))
	}

	roster := &CollaborationRoster{RepoID: r.ID}
	has, err := sess.Get(roster)
	if err != nil {
		return rollback(errors.Newf("get collaboration roster [repo_id: %d]: %v", r.ID, err))
	}
	roster.Content = content
	roster.UpdatedBy = actorID
	roster.UpdatedUnix = time.Now().Unix()
	if has {
		if _, err = sess.ID(roster.ID).AllCols().Update(roster); err != nil {
			return rollback(errors.Newf("update collaboration roster [repo_id: %d]: %v", r.ID, err))
		}
	} else if _, err = sess.Insert(roster); err != nil {
		return rollback(errors.Newf("insert collaboration roster [repo_id: %d]: %v", r.ID, err))
	}

	if err = sess.Commit(); err != nil {
		return nil, errors.Wrap(err, "commit transaction")
	}

	sort.Strings(result.Added)
	sort.Strings(result.ModeChanged)
	sort.Strings(result.Removed)
	sort.Strings(result.SkippedManual)
	return result, nil
}

// GetCollaborationRoster returns the saved roster content of the repository.
// It returns an empty string when no roster has ever been applied.
func (r *Repository) GetCollaborationRoster() (string, error) {
	roster := &CollaborationRoster{RepoID: r.ID}
	has, err := x.Get(roster)
	if err != nil {
		return "", errors.Newf("get collaboration roster [repo_id: %d]: %v", r.ID, err)
	} else if !has {
		return "", nil
	}
	return roster.Content, nil
}
