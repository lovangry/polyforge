package server

import (
	"database/sql"
	"encoding/json"

	"github.com/silgrid/polyforge/internal/core/store"
)

func errNoRowsSentinel() error { return sql.ErrNoRows }

func newID(prefix string) (string, error) { return store.NewID(prefix) }

func newSecret(n int) (string, error) { return store.NewSecret(n) }

func nowStamp() string { return store.Now() }

func jsonRaw(b []byte) json.RawMessage { return json.RawMessage(b) }
