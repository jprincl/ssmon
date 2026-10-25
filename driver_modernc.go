//go:build !cgo_sqlite

package main

// Default SQLite driver: pure Go, no CGO, keeps the binary static and
// cross-compilable. Build with -tags cgo_sqlite to use mattn/go-sqlite3.
import _ "modernc.org/sqlite"

const sqliteDriver = "sqlite"
