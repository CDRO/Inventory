package httpapi

// createUserInput is the body of POST /api/admin/users.
//
// # The one place is_admin appears as a JSON name, and why it is its own file
//
// docs/specs/03-auth-and-multi-tenancy.md takes is_admin as input when an admin
// creates a user, and forbids it as output from every JSON API. A struct tag
// cannot say which direction it serves, so TestIsAdminIsNeverSerialized — which
// scans the source for the name — has to be told about this type. It is told
// about this *file*, not a pattern: nothing else lives here, and the test also
// asserts this file never encodes or writes anything. A response type added
// here to dodge the scan would fail that second check.
type createUserInput struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}
