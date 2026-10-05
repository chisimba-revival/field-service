package store_test

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Two integration packages share one database, and `go test` runs packages in
// parallel. Two suites that both truncate the same tables then collide: the
// first symptom was a duplicate key on a drive one had seeded, and the second
// was a deadlock on truncate itself.
//
// Each suite takes a Postgres advisory lock for the duration of its run, which
// serialises them without a shared file, a shared counter, or anybody having to
// remember to pass -p 1. A separate database per package would work too and is
// the better answer if the suites ever grow enough to be worth the setup.

// IntegrationTestLock is the key every package that shares this database takes
// before it runs.
//
// The same key in every package, which is the entire point: two suites holding
// different keys exclude nobody. The first version gave each package its own
// key and wrote a comment saying that was deliberate, so the suites ran
// concurrently anyway and deadlocked on truncate exactly as they had before.
const IntegrationTestLock int64 = 821004001

// holdAdvisoryLock takes a session-level advisory lock and returns a function
// that releases it.
//
// Session-level rather than transaction-level because the suites truncate and
// commit constantly; a transaction-scoped lock would be released by the first
// commit and would stop excluding anybody. The lock is released explicitly as
// well as by closing the connection, because a lock that outlives its holder
// because of a missed cleanup would make the next run hang rather than fail.
func holdAdvisoryLock(conn *pgx.Conn) func() {
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", IntegrationTestLock); err != nil {
		// A database that cannot be locked is a database that cannot be tested.
		// Failing loudly here beats two suites corrupting each other's fixtures.
		panic("store: taking the integration-test lock: " + err.Error())
	}
	return func() {
		_, _ = conn.Exec(ctx, "select pg_advisory_unlock($1)", IntegrationTestLock)
	}
}
