// Package repository holds the pgx-backed implementations of the domain
// repository interfaces (RFC-0021 P1-2..P1-5): availability reads, admin
// stock commands, and the reservation write-side (Reserve/Release/Commit
// with the movement ledger). Integration tests run via
// go test -tags=integration ./internal/core/repository/...
package repository
