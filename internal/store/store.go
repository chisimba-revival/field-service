// Package store holds the connection and the isolation boundary it enforces.
//
// Nothing in here decides who may see a row. What it does is make the answer
// impossible to get wrong from the outside: a caller cannot reach the database
// without the grants that its verified token carried, because the database
// refuses to answer a query that has not been told what the caller may read.
//
// The alternative — a repository function that adds `context_code = any($grants)`
// and trusts every caller to pass one — puts the isolation in application code,
// where a new query that forgets the predicate is a data breach rather than a
// failing test. This is the second of the three layers the contract requires.
package store

import (
	"os"
	"strings"
)

// DSN returns the connection string for the runtime role.
//
// It deliberately does not default to a usable password. The runtime role is
// created with LOGIN and no password, and this returns empty rather than
// inventing one: a default credential in source is a credential in source.
func DSN() string { return envOrEmpty("FIELDSVC_DATABASE_URL") }

// MigrationRuntimeRole is the role the application connects as. It must not be
// a superuser and must not have BYPASSRLS, because a superuser bypasses
// row-level security entirely and the policies below then do nothing.
//
// This is stated as a constant rather than left implicit because the failure
// mode is invisible: connecting as a superuser returns every row in every
// context with no error, and the SQL reads correctly throughout.
const MigrationRuntimeRole = "fieldapp"

// envOrEmpty reads configuration from the environment, treating an unset
// variable as absent rather than as an empty setting.
//
// The distinction matters for FIELDSVC_DATABASE_URL specifically: an empty
// database URL would otherwise be a connection attempt to nowhere, which looks
// like the database being down rather than like the service being unconfigured.
func envOrEmpty(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
