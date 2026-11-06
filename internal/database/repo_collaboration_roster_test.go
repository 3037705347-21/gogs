package database

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/core"
	"xorm.io/xorm"
)

func TestParseCollaborationRoster(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    []CollaborationRosterEntry
		wantErr RosterValidationErrorKind
	}{
		{
			name:    "empty content",
			content: "",
			want:    []CollaborationRosterEntry{},
		},
		{
			name:    "blank lines and comments are ignored",
			content: "\n  \n# a comment\n\t# indented comment\n",
			want:    []CollaborationRosterEntry{},
		},
		{
			name:    "valid entries are normalized to lower case",
			content: "# team roster\nAlice WRITE\n  bob   Read  \ncarol admin",
			want: []CollaborationRosterEntry{
				{Name: "alice", Mode: AccessModeWrite},
				{Name: "bob", Mode: AccessModeRead},
				{Name: "carol", Mode: AccessModeAdmin},
			},
		},
		{
			name:    "line with too few fields",
			content: "alice",
			wantErr: RosterErrorInvalidEntry,
		},
		{
			name:    "line with too many fields",
			content: "alice write admin",
			wantErr: RosterErrorInvalidEntry,
		},
		{
			name:    "invalid access level",
			content: "alice owner",
			wantErr: RosterErrorInvalidMode,
		},
		{
			name:    "duplicate username",
			content: "alice read\nalice write",
			wantErr: RosterErrorDuplicateUser,
		},
		{
			name:    "duplicate username differs only in case",
			content: "Alice read\nalice write",
			wantErr: RosterErrorDuplicateUser,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, err := ParseCollaborationRoster(test.content)
			if test.wantErr != 0 {
				verr, ok := IsRosterValidationError(err)
				require.True(t, ok, "expected validation error, got %v", err)
				assert.Equal(t, test.wantErr, verr.Kind)
				assert.NotEmpty(t, verr.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, entries)
		})
	}
}

func setupRosterTestEngine(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "collaboration-roster.db")
	engine, err := xorm.NewEngine("sqlite3", "file:"+dbPath+"?cache=shared&mode=rwc")
	require.NoError(t, err)
	engine.SetMapper(core.GonicMapper{})
	// The "access" table lives in the gorm-managed Tables list in production.
	require.NoError(t, engine.StoreEngine("InnoDB").Sync2(append(legacyTables, new(Access))...))

	previous := x
	x = engine
	t.Cleanup(func() {
		_ = engine.Close()
		x = previous
	})
}

func createRosterUser(t *testing.T, name string, typ UserType) *User {
	t.Helper()
	user := &User{
		Name:      name,
		LowerName: strings.ToLower(name),
		Email:     name + "@example.com",
		Type:      typ,
	}
	_, err := x.Insert(user)
	require.NoError(t, err)
	return user
}

func rosterCollaborations(t *testing.T, repoID int64) map[string]*Collaboration {
	t.Helper()
	collaborations := make([]*Collaboration, 0)
	err := x.Find(&collaborations, &Collaboration{RepoID: repoID})
	require.NoError(t, err)

	byName := make(map[string]*Collaboration, len(collaborations))
	for _, collaboration := range collaborations {
		user := new(User)
		has, err := x.ID(collaboration.UserID).Get(user)
		require.NoError(t, err)
		require.True(t, has)
		byName[user.LowerName] = collaboration
	}
	return byName
}

func rosterAccessModes(t *testing.T, repoID int64) map[string]AccessMode {
	t.Helper()
	accesses := make([]*Access, 0)
	err := x.Find(&accesses, &Access{RepoID: repoID})
	require.NoError(t, err)

	byName := make(map[string]AccessMode, len(accesses))
	for _, access := range accesses {
		user := new(User)
		has, err := x.ID(access.UserID).Get(user)
		require.NoError(t, err)
		require.True(t, has)
		byName[user.LowerName] = access.Mode
	}
	return byName
}

func TestRepository_ApplyCollaborationRoster(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	setupRosterTestEngine(t)

	owner := createRosterUser(t, "owner", UserTypeIndividual)
	repo := &Repository{
		OwnerID:   owner.ID,
		Owner:     owner,
		Name:      "test-repo",
		LowerName: "test-repo",
	}
	_, err := x.Insert(repo)
	require.NoError(t, err)

	createRosterUser(t, "alice", UserTypeIndividual)
	createRosterUser(t, "bob", UserTypeIndividual)
	createRosterUser(t, "carol", UserTypeIndividual)
	createRosterUser(t, "dave", UserTypeIndividual)
	createRosterUser(t, "eve", UserTypeIndividual)

	// First application adds listed users and marks them as managed.
	result, err := repo.ApplyCollaborationRoster(owner.ID, "alice write\nbob read\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, result.Added)
	assert.Empty(t, result.ModeChanged)
	assert.Empty(t, result.Removed)
	assert.Empty(t, result.SkippedManual)

	collaborations := rosterCollaborations(t, repo.ID)
	require.Len(t, collaborations, 2)
	assert.Equal(t, AccessModeWrite, collaborations["alice"].Mode)
	assert.Equal(t, AccessModeRead, collaborations["bob"].Mode)
	assert.True(t, collaborations["alice"].ManagedByRoster)
	assert.True(t, collaborations["bob"].ManagedByRoster)

	accessModes := rosterAccessModes(t, repo.ID)
	assert.Equal(t, AccessModeWrite, accessModes["alice"])
	assert.Equal(t, AccessModeRead, accessModes["bob"])

	// Reapplying the same roster does nothing, rows are not duplicated or
	// rewritten.
	result, err = repo.ApplyCollaborationRoster(owner.ID, "alice write\nbob read\n")
	require.NoError(t, err)
	assert.Empty(t, result.Added)
	assert.Empty(t, result.ModeChanged)
	assert.Empty(t, result.Removed)
	assert.Empty(t, result.SkippedManual)
	assert.Len(t, rosterCollaborations(t, repo.ID), 2)

	// Carol is added through the manual entry point like an admin would do.
	carolUser, err := getUserByID(x, mustUserID(t, "carol"))
	require.NoError(t, err)
	require.NoError(t, repo.AddCollaborator(carolUser))

	// Roster changes: alice is promoted, dave is added, bob stays, carol is
	// not listed and must stay untouched.
	result, err = repo.ApplyCollaborationRoster(owner.ID, "alice admin\nbob read\ndave read")
	require.NoError(t, err)
	assert.Equal(t, []string{"dave"}, result.Added)
	assert.Equal(t, []string{"alice"}, result.ModeChanged)
	assert.Empty(t, result.Removed)
	assert.Empty(t, result.SkippedManual)

	collaborations = rosterCollaborations(t, repo.ID)
	require.Len(t, collaborations, 4)
	assert.Equal(t, AccessModeAdmin, collaborations["alice"].Mode)
	assert.Equal(t, AccessModeRead, collaborations["dave"].Mode)
	assert.True(t, collaborations["dave"].ManagedByRoster)
	assert.Equal(t, AccessModeWrite, collaborations["carol"].Mode)
	assert.False(t, collaborations["carol"].ManagedByRoster)

	// Shrinking the roster only removes users it previously managed. Manual
	// collaborators stay in place.
	result, err = repo.ApplyCollaborationRoster(owner.ID, "dave read")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, result.Removed)
	assert.Empty(t, result.Added)
	assert.Empty(t, result.ModeChanged)

	collaborations = rosterCollaborations(t, repo.ID)
	assert.Contains(t, collaborations, "dave")
	assert.Contains(t, collaborations, "carol")
	assert.NotContains(t, collaborations, "alice")
	assert.NotContains(t, collaborations, "bob")
	assert.Equal(t, AccessModeWrite, collaborations["carol"].Mode)

	// Eve is new and gets added, while carol already exists as a manual
	// collaborator and must be skipped even though the roster lists a lower
	// level.
	result, err = repo.ApplyCollaborationRoster(owner.ID, "dave read\neve read\ncarol read")
	require.NoError(t, err)
	assert.Equal(t, []string{"eve"}, result.Added)
	assert.Empty(t, result.ModeChanged)
	assert.Equal(t, []string{"carol"}, result.SkippedManual)

	collaborations = rosterCollaborations(t, repo.ID)
	assert.Equal(t, AccessModeWrite, collaborations["carol"].Mode)
	assert.False(t, collaborations["carol"].ManagedByRoster)

	// Invalid rosters fail without changing any permission or the saved
	// roster.
	before := rosterCollaborations(t, repo.ID)
	for _, invalid := range []string{
		"dave read\nnobody write",
		"dave owner",
		"dave read\ndave admin",
		"owner admin",
	} {
		_, err = repo.ApplyCollaborationRoster(owner.ID, invalid)
		verr, ok := IsRosterValidationError(err)
		require.True(t, ok, "content %q: expected validation error, got %v", invalid, err)
		assert.NotZero(t, verr.Kind)
		assert.Equal(t, before, rosterCollaborations(t, repo.ID), "content %q changed permissions", invalid)
	}

	saved, err := repo.GetCollaborationRoster()
	require.NoError(t, err)
	assert.Equal(t, "dave read\neve read\ncarol read", saved)

	// An empty roster removes every managed collaborator but keeps manual
	// ones.
	result, err = repo.ApplyCollaborationRoster(owner.ID, "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"dave", "eve"}, result.Removed)
	collaborations = rosterCollaborations(t, repo.ID)
	assert.Len(t, collaborations, 1)
	assert.Contains(t, collaborations, "carol")
	assert.False(t, collaborations["carol"].ManagedByRoster)
}

func mustUserID(t *testing.T, name string) int64 {
	t.Helper()
	user := &User{LowerName: name}
	has, err := x.Get(user)
	require.NoError(t, err)
	require.True(t, has, "user %q does not exist", name)
	return user.ID
}
