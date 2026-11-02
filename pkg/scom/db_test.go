package scom

import (
	"database/sql"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// Go's default pool keeps only 2 idle connections, which made every busy
// dashboard refresh open new SQL logins.
func TestConfigurePool(t *testing.T) {
	connector, err := mssql.NewConnector("sqlserver://user:pass@localhost?database=OperationsManager")
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector) // doesn't connect until used
	defer func() { _ = db.Close() }()

	configurePool(db)
	if got := db.Stats().MaxOpenConnections; got != maxOpenConnsPerDB {
		t.Errorf("MaxOpenConnections = %d, want %d", got, maxOpenConnsPerDB)
	}
}
