// Package repository will hold the pgx-backed implementations of the domain
// repository interfaces (balances, reservations, movements — RFC-0021
// P1-2..P1-5). It exists in the skeleton so the shared integration-test job
// (go test -tags=integration ./internal/core/repository/...) resolves the
// package path; "no test files" is a pass, a missing directory is not.
package repository
