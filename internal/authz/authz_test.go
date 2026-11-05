package authz

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
)

// planTable is plan section 14 written out cell by cell, independently of
// Matrix, so a change to either fails this test. Columns: platform_admin,
// registry_admin, owner, maintainer, contributor, viewer.
var planTable = []struct {
	perm  Permission
	cells [6]bool
}{
	{ManageUsers, [6]bool{true, false, false, false, false, false}},
	{ManageGlobalRegistry, [6]bool{true, true, false, false, false, false}},
	{ManageProjectRegistry, [6]bool{true, false, true, true, false, false}},
	{ReadRegistry, [6]bool{true, true, true, true, true, true}},
	{ManageProjectMembers, [6]bool{true, false, true, false, false, false}},
	{EditConfiguration, [6]bool{true, false, true, true, false, false}},
	{ActivateConfiguration, [6]bool{true, false, true, false, false, false}},
	{ManageSources, [6]bool{true, false, true, true, true, false}},
	{RecordRights, [6]bool{true, false, true, true, false, false}},
	{ReadProjectData, [6]bool{true, false, true, true, true, true}},
	{ReadProjectAudit, [6]bool{true, false, true, false, false, false}},
	{ReadGlobalAudit, [6]bool{true, false, false, false, false, false}},
}

func principal(role GlobalRole) Principal {
	return Principal{ID: uuid.New(), Kind: audit.User, GlobalRole: role, Scopes: []Scope{InstallationScope}}
}

func TestMatrixMatchesPlanTable(t *testing.T) {
	project := uuid.New()
	globals := []GlobalRole{PlatformAdmin, RegistryAdmin}
	projects := []ProjectRole{Owner, Maintainer, Contributor, Viewer}
	seen := map[Permission]bool{}
	for _, row := range planTable {
		seen[row.perm] = true
		for i, g := range globals {
			// A global role holds the permission in any project, member or not.
			if got := Allowed(principal(g), row.perm, InProject(project, "")); got != row.cells[i] {
				t.Errorf("%s / %s = %v, plan says %v", row.perm, g, got, row.cells[i])
			}
		}
		for i, pr := range projects {
			// A plain member holds a project permission only through its
			// role in that project.
			if got := Allowed(principal(Member), row.perm, InProject(project, pr)); got != row.cells[2+i] {
				t.Errorf("%s / %s = %v, plan says %v", row.perm, pr, got, row.cells[2+i])
			}
		}
	}
	seen[CreateProject] = true // asserted separately: the cell reads "any member (U7)"
	for perm := range Matrix {
		if !seen[perm] {
			t.Errorf("permission %s has no row in the plan table test", perm)
		}
	}
}

func TestCreateProjectIsAnyMember(t *testing.T) {
	for role, want := range map[GlobalRole]bool{PlatformAdmin: true, Member: true, RegistryAdmin: false} {
		if got := Allowed(principal(role), CreateProject, Global()); got != want {
			t.Errorf("create project / %s = %v, want %v", role, got, want)
		}
	}
}

func TestDenyByDefault(t *testing.T) {
	admin := principal(PlatformAdmin)
	project := uuid.New()
	cases := map[string]struct {
		p    Principal
		perm Permission
		t    Target
	}{
		"unknown permission (X1 operations are not mapped)": {admin, Permission("freeze_dataset_version"), Global()},
		"zero target":                      {admin, ManageUsers, Target{}},
		"scope the principal lacks":        {admin, ManageUsers, Target{Scope: "workspace:other"}},
		"no scopes":                        {Principal{ID: admin.ID, Kind: audit.User, GlobalRole: PlatformAdmin}, ManageUsers, Global()},
		"system principal":                 {Principal{ID: admin.ID, Kind: audit.System, GlobalRole: PlatformAdmin, Scopes: admin.Scopes}, ManageUsers, Global()},
		"unknown global role":              {principal("superuser"), ManageUsers, Global()},
		"project role outside its project": {principal(Member), ReadProjectData, Global()},
		"unknown project role":             {principal(Member), ReadProjectData, InProject(project, "admin")},
		"member, global permission":        {principal(Member), ManageUsers, InProject(project, Owner)},
	}
	for name, c := range cases {
		if Allowed(c.p, c.perm, c.t) {
			t.Errorf("%s: allowed", name)
		}
	}
	if !Allowed(principal(PlatformAdmin), ManageUsers, Global()) {
		t.Error("platform_admin must manage users")
	}
	sa := principal(PlatformAdmin)
	sa.Kind = audit.ServiceAccount
	if !Allowed(sa, ManageUsers, Global()) {
		t.Error("a service account holds the permissions of its global role")
	}
}

type recorder struct {
	events []audit.Event
	err    error
}

func (r *recorder) Add(ev audit.Event) error { r.events = append(r.events, ev); return r.err }

func TestAuthorizeRecordsDenials(t *testing.T) {
	rec := &recorder{}
	a := NewAuthorizer(rec)
	member := principal(Member)
	member.AuthMethod = "session"
	d := Denial{Action: "user.created", TargetType: "user", RequestID: "req-9"}

	if err := a.Authorize(principal(PlatformAdmin), ManageUsers, Global(), d); err != nil {
		t.Fatalf("allowed call: %v", err)
	}
	if len(rec.events) != 0 {
		t.Fatal("an allowed call must not record anything")
	}
	if err := a.Authorize(member, ManageUsers, Global(), d); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied call: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Outcome != audit.Denied || ev.Action != "user.created" || ev.ActorPrincipalID != member.ID.String() ||
		ev.ActorKind != audit.User || ev.AuthMethod != "session" || ev.RequestID != "req-9" ||
		ev.Details.(audit.DeniedDetails).Permission != string(ManageUsers) {
		t.Errorf("unexpected event %+v", ev)
	}

	rec.err = errors.New("buffer closed")
	if err := a.Authorize(member, ManageUsers, Global(), d); !errors.Is(err, ErrDenied) || !errors.Is(err, ErrDenialNotRecorded) {
		t.Fatalf("a denial that cannot be recorded must still deny, and say so: %v", err)
	}
}
