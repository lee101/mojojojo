package main

import (
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/lib/pq"
)

var postgresTests struct {
	once    sync.Once
	mu      sync.Mutex
	admin   *sql.DB
	db      *sql.DB
	name    string
	initErr error
}

// testPostgres serializes database-backed tests through one real PostgreSQL
// database. Creating and fsyncing an entire database per test is needlessly
// slow; TRUNCATE gives the same isolation while the schema is migrated once.
func testPostgres(t *testing.T) *sql.DB {
	t.Helper()
	postgresTests.once.Do(initPostgresTests)
	if postgresTests.initErr != nil {
		t.Skipf("PostgreSQL integration tests require a local server: %v", postgresTests.initErr)
	}
	postgresTests.mu.Lock()
	if _, err := postgresTests.db.Exec(`TRUNCATE TABLE
		email_campaign_sends, email_campaign_enrollments, email_preferences,
		stripe_credit_grants, stripe_checkouts, stripe_subscriptions, stripe_customers,
		agent_usage, agent_accrual, capacity_reservations,
		vector_usage, vector_accrual, vector_documents, vector_projects,
		showcase_submissions, app_room_signals, app_room_peers, app_function_usage, project_versions, project_files,
		assets, deployments, projects, edge_tickets, seen, grants, runs, accrual,
		credit_ledger, api_keys, sessions, users
		RESTART IDENTITY CASCADE`); err != nil {
		postgresTests.mu.Unlock()
		t.Fatal(err)
	}
	t.Cleanup(postgresTests.mu.Unlock)
	return postgresTests.db
}

func initPostgresTests() {
	adminURL := os.Getenv("MOJOJOJO_TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = "postgres:///postgres?host=/var/run/postgresql&sslmode=disable"
	}
	admin, err := sql.Open("postgres", adminURL)
	if err != nil {
		postgresTests.initErr = err
		return
	}
	if err := admin.Ping(); err != nil {
		postgresTests.initErr = err
		_ = admin.Close()
		return
	}
	name := "mojojojo_test_" + stringsForDatabase(randomToken(10))
	if _, err := admin.Exec("CREATE DATABASE " + pq.QuoteIdentifier(name)); err != nil {
		postgresTests.initErr = err
		_ = admin.Close()
		return
	}
	db, err := sql.Open("postgres", fmt.Sprintf("postgres:///%s?host=/var/run/postgresql&sslmode=disable", name))
	if err == nil {
		err = db.Ping()
	}
	if err == nil {
		err = migrateDatabase(db)
	}
	if err != nil {
		postgresTests.initErr = err
		if db != nil {
			_ = db.Close()
		}
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(name))
		_ = admin.Close()
		return
	}
	postgresTests.admin, postgresTests.db, postgresTests.name = admin, db, name
}

func TestMain(m *testing.M) {
	os.Unsetenv("EDGE_URL")
	os.Unsetenv("EDGE_TOKEN")
	code := m.Run()
	if postgresTests.db != nil {
		_ = postgresTests.db.Close()
		_, _ = postgresTests.admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, postgresTests.name)
		_, _ = postgresTests.admin.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(postgresTests.name))
		_ = postgresTests.admin.Close()
	}
	os.Exit(code)
}

func stringsForDatabase(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}
