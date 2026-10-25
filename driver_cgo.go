//go:build cgo_sqlite

package main

// Alternative SQLite driver (CGO, needs a C compiler): go build -tags cgo_sqlite
import _ "github.com/mattn/go-sqlite3"

const sqliteDriver = "sqlite3"
