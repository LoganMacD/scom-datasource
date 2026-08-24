package scom

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/a-logan/scom-datasource/pkg/models"
	mssql "github.com/microsoft/go-mssqldb"
)

// sessionInitSQL sets the ANSI/ARITHABORT options SQL Server requires to use
// indexed views, which the SCOM OperationsManager and DataWarehouse databases
// rely on heavily. SSMS enables these by default; the go-mssqldb driver does
// not, so without this, queries against affected tables can silently lose
// access to the indexed views and fall back to plans expensive enough to
// exhaust the optimizer's resources (e.g. "the query processor ran out of
// internal resources") even though the identical query text succeeds in
// SSMS. It is applied via Connector.SessionInitSQL so it runs on every new
// physical connection and on every pooled connection reset, not just once.
const sessionInitSQL = `SET ANSI_NULLS ON;
SET ANSI_PADDING ON;
SET ANSI_WARNINGS ON;
SET ARITHABORT ON;
SET CONCAT_NULL_YIELDS_NULL ON;
SET NUMERIC_ROUNDABORT OFF;
SET QUOTED_IDENTIFIER ON;`

func openWithSessionInit(dsn string) (*sql.DB, error) {
	connector, err := mssql.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	connector.SessionInitSQL = sessionInitSQL
	return sql.OpenDB(connector), nil
}

// DB pairs the two SQL Server connections a SCOM data source instance needs:
// the Operational database (class/group/instance browsing, alerts, health)
// and the Data Warehouse (performance history).
type DB struct {
	Operational *sql.DB
	Warehouse   *sql.DB
}

func Open(settings *models.PluginSettings) (*DB, error) {
	opDSN, err := BuildDSN(settings.Operational, settings.Secrets.OperationalPassword)
	if err != nil {
		return nil, fmt.Errorf("operational connection: %w", err)
	}
	opDB, err := openWithSessionInit(opDSN)
	if err != nil {
		return nil, fmt.Errorf("operational connection: %w", err)
	}

	dwDSN, err := BuildDSN(settings.Warehouse, settings.Secrets.WarehousePassword)
	if err != nil {
		opDB.Close()
		return nil, fmt.Errorf("warehouse connection: %w", err)
	}
	dwDB, err := openWithSessionInit(dwDSN)
	if err != nil {
		opDB.Close()
		return nil, fmt.Errorf("warehouse connection: %w", err)
	}

	return &DB{Operational: opDB, Warehouse: dwDB}, nil
}

func (db *DB) Close() {
	if db.Operational != nil {
		db.Operational.Close()
	}
	if db.Warehouse != nil {
		db.Warehouse.Close()
	}
}

// CheckHealth pings both connections and runs a cheap query against a known
// table/view on each so schema or permission problems surface immediately
// rather than only on first real query.
func (db *DB) CheckHealth(ctx context.Context) error {
	if err := db.Operational.PingContext(ctx); err != nil {
		return fmt.Errorf("operational database: %w", err)
	}
	if err := probe(ctx, db.Operational, "SELECT TOP 1 BaseManagedEntityId FROM dbo.BaseManagedEntity"); err != nil {
		return fmt.Errorf("operational database (dbo.BaseManagedEntity): %w", err)
	}

	if err := db.Warehouse.PingContext(ctx); err != nil {
		return fmt.Errorf("data warehouse: %w", err)
	}
	if err := probe(ctx, db.Warehouse, "SELECT TOP 1 PerformanceRuleInstanceRowId FROM Perf.vPerfHourly"); err != nil {
		return fmt.Errorf("data warehouse (Perf.vPerfHourly): %w", err)
	}

	return nil
}

func probe(ctx context.Context, conn *sql.DB, query string) error {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	return rows.Err()
}
