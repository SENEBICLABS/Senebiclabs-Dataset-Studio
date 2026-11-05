package authz

// Permission is one row of the plan section 14 authorization table.
type Permission string

// Permissions, one per row of the plan 14 table, in table order.
const (
	ManageUsers           Permission = "manage_users"            // Manage users and service accounts
	ManageGlobalRegistry  Permission = "manage_global_registry"  // Publish or draft global registry versions
	ManageProjectRegistry Permission = "manage_project_registry" // Project-private registry entries
	ReadRegistry          Permission = "read_registry"           // Read registry
	CreateProject         Permission = "create_project"          // Create project
	ManageProjectMembers  Permission = "manage_project_members"  // Manage members, archive
	EditConfiguration     Permission = "edit_configuration"      // Configurations: edit
	ActivateConfiguration Permission = "activate_configuration"  // Configurations: activate
	ManageSources         Permission = "manage_sources"          // Sources, artifacts, links
	RecordRights          Permission = "record_rights"           // Record rights statements
	ReadProjectData       Permission = "read_project_data"       // Read project data
	ReadProjectAudit      Permission = "read_project_audit"      // Read audit: project
	ReadGlobalAudit       Permission = "read_global_audit"       // Read audit: global
)

// Rule lists who holds a permission. Global roles hold it everywhere;
// project roles hold it only inside their project, and only when
// ProjectScoped is set.
type Rule struct {
	Global        []GlobalRole
	ProjectScoped bool
	Project       []ProjectRole
}

// Matrix is the plan section 14 table, transcribed row by row. A permission
// that is not a key here is denied to everyone.
//
// Not in the matrix: the Rev 2 operations (records, revisions, batch, dataset
// versions, freeze, cancel, retract, rebuild). Their role mapping is
// Unresolved X1 (plan 20.3, needed by M12) and is deliberately not inferred,
// so those operations are denied by default until it is decided.
var Matrix = map[Permission]Rule{
	ManageUsers:          {Global: []GlobalRole{PlatformAdmin}},
	ManageGlobalRegistry: {Global: []GlobalRole{PlatformAdmin, RegistryAdmin}},
	ManageProjectRegistry: {
		Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true,
		Project: []ProjectRole{Owner, Maintainer},
	},
	// Transcribed literally: both global admin roles, and every project
	// role. Whether a global "member" outside any project may read the
	// global registry is not stated; it is denied until confirmed (no
	// registry route exists before M8).
	ReadRegistry: {
		Global: []GlobalRole{PlatformAdmin, RegistryAdmin}, ProjectScoped: true,
		Project: []ProjectRole{Owner, Maintainer, Contributor, Viewer},
	},
	// "any member" (approved default U7): global role member. The
	// registry_admin column is blank in the table and is read literally.
	CreateProject:        {Global: []GlobalRole{PlatformAdmin, Member}},
	ManageProjectMembers: {Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true, Project: []ProjectRole{Owner}},
	EditConfiguration: {
		Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true,
		Project: []ProjectRole{Owner, Maintainer},
	},
	ActivateConfiguration: {Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true, Project: []ProjectRole{Owner}},
	ManageSources: {
		Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true,
		Project: []ProjectRole{Owner, Maintainer, Contributor},
	},
	RecordRights: {
		Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true,
		Project: []ProjectRole{Owner, Maintainer},
	},
	ReadProjectData: {
		Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true,
		Project: []ProjectRole{Owner, Maintainer, Contributor, Viewer},
	},
	ReadProjectAudit: {Global: []GlobalRole{PlatformAdmin}, ProjectScoped: true, Project: []ProjectRole{Owner}},
	ReadGlobalAudit:  {Global: []GlobalRole{PlatformAdmin}},
}
