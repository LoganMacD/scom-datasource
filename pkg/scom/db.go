package scom

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

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

// Connection pool limits, per database, per data source.
//
// Go's default keeps only 2 idle connections, so every refresh of a busy
// dashboard opened a new SQL login (TLS plus NTLM/Kerberos) for each
// concurrent query beyond 2. Keeping more idle connections avoids that. The
// cap on open connections makes a burst of panels queue for a connection
// instead of flooding the SCOM SQL Server, and the lifetime limit moves
// connections over after a failover or DNS change.
const (
	maxOpenConnsPerDB = 20
	maxIdleConnsPerDB = 10
	connMaxIdleTime   = 10 * time.Minute
	connMaxLifetime   = time.Hour
)

func openWithSessionInit(dsn string) (*sql.DB, error) {
	connector, err := mssql.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	connector.SessionInitSQL = sessionInitSQL
	db := sql.OpenDB(connector)
	configurePool(db)
	return db, nil
}

func configurePool(db *sql.DB) {
	db.SetMaxOpenConns(maxOpenConnsPerDB)
	db.SetMaxIdleConns(maxIdleConnsPerDB)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	db.SetConnMaxLifetime(connMaxLifetime)
}

// DB pairs the two SQL Server connections a SCOM data source instance needs:
// the Operational database (class/group/instance browsing, alerts, health)
// and the Data Warehouse (performance history), plus the data source's
// cached lookups (see dbCaches).
type DB struct {
	Operational *sql.DB
	Warehouse   *sql.DB
	caches      *dbCaches
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
		_ = opDB.Close()
		return nil, fmt.Errorf("warehouse connection: %w", err)
	}
	dwDB, err := openWithSessionInit(dwDSN)
	if err != nil {
		_ = opDB.Close()
		return nil, fmt.Errorf("warehouse connection: %w", err)
	}

	return &DB{Operational: opDB, Warehouse: dwDB, caches: newDBCaches()}, nil
}

// cache returns the data source's caches. A DB built without Open (in tests)
// has none, and every lookup then goes straight to the database.
func (db *DB) cache() *dbCaches {
	if db.caches == nil {
		return &dbCaches{}
	}
	return db.caches
}

func (db *DB) Close() {
	if db.Operational != nil {
		_ = db.Operational.Close()
	}
	if db.Warehouse != nil {
		_ = db.Warehouse.Close()
	}
}

// HealthReport is the result of DB.CheckHealth. Operational and Warehouse are
// nil when that database is reachable and looks like the right SCOM
// database. RetentionErr is set when the warehouse works but its retention
// settings can't be read, which only affects Auto aggregation (it falls back
// to hourly or daily data).
type HealthReport struct {
	Operational  error
	Warehouse    error
	RetentionErr error
}

var errNoRetentionRows = errors.New("dbo.StandardDatasetAggregation has no rows for the Perf dataset")

// CheckHealth checks both databases at the same time, so the config page's
// Save & test reports both rather than stopping at the first failure. Each
// check pings the server and reads a known table or view, so wrong database
// names and missing permissions show up here, not on the first dashboard.
func (db *DB) CheckHealth(ctx context.Context) HealthReport {
	var report HealthReport
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		report.Operational = checkDatabase(ctx, db.Operational, "dbo.BaseManagedEntity",
			"SELECT TOP 1 BaseManagedEntityId FROM dbo.BaseManagedEntity")
	}()
	go func() {
		defer wg.Done()
		report.Warehouse = checkDatabase(ctx, db.Warehouse, "Perf.vPerfHourly",
			"SELECT TOP 1 PerformanceRuleInstanceRowId FROM Perf.vPerfHourly")
		if report.Warehouse != nil {
			return
		}
		retention, err := loadPerfRetention(ctx, db.Warehouse)
		if err == nil && len(retention) == 0 {
			err = errNoRetentionRows
		}
		report.RetentionErr = err
	}()
	wg.Wait()
	return report
}

func checkDatabase(ctx context.Context, conn *sql.DB, object, query string) error {
	if err := conn.PingContext(ctx); err != nil {
		return err
	}
	if err := probe(ctx, conn, query); err != nil {
		return fmt.Errorf("can't read %s: %w", object, err)
	}
	return nil
}

func probe(ctx context.Context, conn *sql.DB, query string) error {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	return rows.Err()
}
