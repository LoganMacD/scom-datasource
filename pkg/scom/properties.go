package scom

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	mssql "github.com/microsoft/go-mssqldb"
)

// SCOM auto-generates a SQL table (and, for many classes, a view joining the
// full base-class inheritance chain) per management-pack class in the
// Operational DB, exposing every MP-defined typed property as its own
// column plus a BaseManagedEntityId column. Rather than guess the name from
// TypeName, dbo.ManagedType stores it directly: ManagedTypeViewName (when
// present — the inheritance-joined view, so it also carries inherited
// properties) or ManagedTypeTableName (this class's own added properties
// only). A class with neither (abstract, or one that adds no typed
// properties beyond its base classes) has no per-class properties at all.
//
// This looks up by TypeName, which — like groupInstancesByClass's grouping
// below — isn't guaranteed globally unique (only unique per management
// pack); an ambiguous match silently picks one. Not fixing that here since
// it predates this function, but worth knowing if a lookup for a
// multiply-defined class name resolves to the wrong management pack's view.
func resolveClassView(ctx context.Context, db *sql.DB, typeName string) (string, error) {
	var viewName, tableName sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT ManagedTypeViewName, ManagedTypeTableName FROM dbo.ManagedType WHERE TypeName = @typeName`,
		sql.Named("typeName", typeName),
	).Scan(&viewName, &tableName)
	if err != nil {
		return "", fmt.Errorf("resolve class view for %s: %w", typeName, err)
	}
	if viewName.Valid && viewName.String != "" {
		return viewName.String, nil
	}
	if tableName.Valid && tableName.String != "" {
		return tableName.String, nil
	}
	return "", nil
}

func quoteIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// resolveTypeName looks a class id up by its uniqueidentifier column, so SQL
// Server can seek on it (converting the column to text for every row couldn't
// use the index). classID must already be a valid GUID (see ValidateIDs).
func resolveTypeName(ctx context.Context, db *sql.DB, classID string) (string, error) {
	var typeName string
	err := db.QueryRowContext(ctx,
		`SELECT TypeName FROM dbo.ManagedType WHERE ManagedTypeId = CONVERT(uniqueidentifier, @classId)`,
		sql.Named("classId", classID),
	).Scan(&typeName)
	if err != nil {
		return "", fmt.Errorf("resolve class: %w", err)
	}
	return typeName, nil
}

// classViewColumns introspects the per-class view's columns, excluding
// identity/internal columns that aren't meaningful "properties".
func classViewColumns(ctx context.Context, db *sql.DB, viewName string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COLUMN_NAME
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA = 'dbo' AND TABLE_NAME = @viewName
ORDER BY ORDINAL_POSITION`, sql.Named("viewName", viewName))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	skip := map[string]bool{"BaseManagedEntityId": true}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		if !skip[col] {
			cols = append(cols, col)
		}
	}
	return cols, rows.Err()
}

// classPropertyLists is one class's typed-property view and its columns (see
// resolveClassView). ViewName is "" for a class with no typed properties.
type classPropertyLists struct {
	TypeName string
	ViewName string
	Columns  []string
}

// classPropertiesByType returns a class's property view and columns, cached
// for metadataCacheTTL: they only change when a management pack is imported.
func (db *DB) classPropertiesByType(ctx context.Context, typeName string) (classPropertyLists, error) {
	return db.cache().classProps.get(ctx, cacheKey("type", typeName), metadataCacheTTL,
		func(ctx context.Context) (classPropertyLists, error) {
			viewName, err := resolveClassView(ctx, db.Operational, typeName)
			if err != nil {
				return classPropertyLists{}, err
			}
			props := classPropertyLists{TypeName: typeName, ViewName: viewName}
			if viewName != "" {
				cols, err := classViewColumns(ctx, db.Operational, viewName)
				if err != nil {
					return classPropertyLists{}, fmt.Errorf("list properties for %s: %w", typeName, err)
				}
				props.Columns = cols
			}
			return props, nil
		})
}

// classPropertiesByID is classPropertiesByType for a class id.
func (db *DB) classPropertiesByID(ctx context.Context, classID string) (classPropertyLists, error) {
	return db.cache().classProps.get(ctx, cacheKey("class", strings.ToLower(classID)), metadataCacheTTL,
		func(ctx context.Context) (classPropertyLists, error) {
			typeName, err := resolveTypeName(ctx, db.Operational, classID)
			if err != nil {
				return classPropertyLists{}, err
			}
			return db.classPropertiesByType(ctx, typeName)
		})
}

// baseEntityColumns are the dbo.BaseManagedEntity columns offered for every
// class, in the order the picker lists them.
var baseEntityColumns = []string{"DisplayName", "FullName", "Path", "IsManaged"}

// ListPropertyNames backs the /properties resource endpoint used by the
// query editor's property picker.
func ListPropertyNames(ctx context.Context, db *DB, classID string) ([]Option, error) {
	props, err := db.classPropertiesByID(ctx, classID)
	if err != nil {
		return nil, err
	}
	out := make([]Option, 0, len(baseEntityColumns)+len(props.Columns))
	for _, c := range baseEntityColumns {
		out = append(out, Option{Value: c, Label: c})
	}
	for _, c := range props.Columns {
		out = append(out, Option{Value: c, Label: c})
	}
	return out, nil
}

// QueryProperties returns one frame per class among the selected instances,
// with one row per instance and one field per requested property. Instances
// are grouped by class because typed properties live in per-class views.
// Frames come back sorted by class name and rows by instance name, so a
// table's frame picker and row order stay put across refreshes.
func QueryProperties(ctx context.Context, db *DB, instanceIDs []string, propertyNames []string) ([]*data.Frame, error) {
	if len(instanceIDs) == 0 {
		return nil, nil
	}

	groups, err := groupInstancesByClass(ctx, db.Operational, instanceIDs)
	if err != nil {
		return nil, err
	}
	typeNames := make([]string, 0, len(groups))
	for typeName := range groups {
		typeNames = append(typeNames, typeName)
	}
	slices.Sort(typeNames)

	frames := make([]*data.Frame, 0, len(typeNames))
	for _, typeName := range typeNames {
		frame, err := queryPropertiesForClass(ctx, db, typeName, groups[typeName], propertyNames)
		if err != nil {
			return nil, fmt.Errorf("properties for class %s: %w", typeName, err)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

// propertiesScopeTempTable is the temp table groupInstancesByClass/
// queryPropertiesForClass populate with instanceIDs, joined via a subquery
// rather than folded into a literal IN list — a class/group selection can be
// large enough to blow past SQL Server's per-query parameter limit. See
// idScopeTempTable and the same fix in counters.go/performance.go/alerts.go/
// health.go.
const propertiesScopeTempTable = "#PropertyScope"

func groupInstancesByClass(ctx context.Context, db *sql.DB, instanceIDs []string) (map[string][]string, error) {
	setupSQL, args := idScopeTempTable(propertiesScopeTempTable, instanceIDs)
	rows, err := db.QueryContext(ctx, setupSQL+fmt.Sprintf(`
SELECT CONVERT(varchar(64), bme.BaseManagedEntityId), mt.TypeName
FROM dbo.BaseManagedEntity bme
INNER JOIN dbo.ManagedType mt ON bme.BaseManagedTypeId = mt.ManagedTypeId
WHERE bme.BaseManagedEntityId IN (SELECT Id FROM %s)`, propertiesScopeTempTable), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	groups := map[string][]string{}
	for rows.Next() {
		var id, typeName string
		if err := rows.Scan(&id, &typeName); err != nil {
			return nil, err
		}
		groups[typeName] = append(groups[typeName], id)
	}
	return groups, rows.Err()
}

// buildPropertiesQuery selects the requested properties of instanceIDs from
// dbo.BaseManagedEntity and, when needed, the class's property view. Only
// names that are a known base column or an introspected column of the view
// are ever selected — a requested name is never interpolated into SQL as is.
// No valid names selects every property.
func buildPropertiesQuery(props classPropertyLists, instanceIDs, requested []string) (string, []any) {
	available := map[string]bool{}
	for _, c := range props.Columns {
		available[c] = true
	}
	isBase := func(name string) bool { return slices.Contains(baseEntityColumns, name) }

	var selected []string
	for _, name := range requested {
		if isBase(name) || available[name] {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		selected = append([]string{"DisplayName"}, props.Columns...)
	}

	needsClassView := false
	selectCols := make([]string, len(selected))
	for i, name := range selected {
		if isBase(name) {
			selectCols[i] = "bme." + quoteIdent(name)
		} else {
			selectCols[i] = "cv." + quoteIdent(name)
			needsClassView = true
		}
	}

	setupSQL, args := idScopeTempTable(propertiesScopeTempTable, instanceIDs)
	query := setupSQL + fmt.Sprintf(`
SELECT bme.DisplayName AS InstanceDisplayName, %s
FROM dbo.BaseManagedEntity bme`, strings.Join(selectCols, ", "))
	if needsClassView {
		query += fmt.Sprintf(`
INNER JOIN dbo.%s cv ON cv.BaseManagedEntityId = bme.BaseManagedEntityId`, quoteIdent(props.ViewName))
	}
	query += fmt.Sprintf(`
WHERE bme.BaseManagedEntityId IN (SELECT Id FROM %s)
ORDER BY bme.DisplayName`, propertiesScopeTempTable)
	return query, args
}

func queryPropertiesForClass(ctx context.Context, db *DB, typeName string, instanceIDs []string, requested []string) (*data.Frame, error) {
	props, err := db.classPropertiesByType(ctx, typeName)
	if err != nil {
		return nil, err
	}

	query, args := buildPropertiesQuery(props, instanceIDs, requested)
	rows, err := db.Operational.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	columns := make([]*propertyColumn, len(colTypes))
	targets := make([]any, len(colTypes))
	for i, ct := range colTypes {
		columns[i] = &propertyColumn{name: ct.Name(), kind: propertyKindOf(ct.DatabaseTypeName())}
		targets[i] = columns[i].target()
	}

	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		for _, c := range columns {
			c.take()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frame := data.NewFrame(typeName)
	for _, c := range columns {
		frame.Fields = append(frame.Fields, c.field())
	}
	return frame, nil
}

// propertyKind is how a property column is typed in the frame, so numbers
// sort and threshold as numbers and dates display as dates.
type propertyKind int

const (
	propertyString propertyKind = iota
	propertyInt
	propertyFloat
	propertyBool
	propertyTime
	propertyGUID
)

func propertyKindOf(dbType string) propertyKind {
	switch strings.ToUpper(dbType) {
	case "TINYINT", "SMALLINT", "INT", "BIGINT":
		return propertyInt
	case "FLOAT", "REAL", "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		return propertyFloat
	case "BIT":
		return propertyBool
	case "DATETIME", "DATETIME2", "SMALLDATETIME", "DATE", "DATETIMEOFFSET":
		return propertyTime
	case "UNIQUEIDENTIFIER":
		return propertyGUID
	default:
		return propertyString
	}
}

// propertyColumn scans one column, row by row, into a typed field. NULLs
// become empty strings in text columns (as before) and nulls elsewhere.
type propertyColumn struct {
	name string
	kind propertyKind

	strs   []string
	ints   []*int64
	floats []*float64
	bools  []*bool
	times  []*time.Time

	ns sql.NullString
	ni sql.NullInt64
	nf sql.NullFloat64
	nb sql.NullBool
	nt sql.NullTime
	ng mssql.NullUniqueIdentifier
}

func (c *propertyColumn) target() any {
	switch c.kind {
	case propertyInt:
		return &c.ni
	case propertyFloat:
		return &c.nf
	case propertyBool:
		return &c.nb
	case propertyTime:
		return &c.nt
	case propertyGUID:
		return &c.ng
	default:
		return &c.ns
	}
}

func (c *propertyColumn) take() {
	switch c.kind {
	case propertyInt:
		c.ints = append(c.ints, nullable(c.ni.Int64, c.ni.Valid))
	case propertyFloat:
		c.floats = append(c.floats, nullable(c.nf.Float64, c.nf.Valid))
	case propertyBool:
		c.bools = append(c.bools, nullable(c.nb.Bool, c.nb.Valid))
	case propertyTime:
		c.times = append(c.times, nullable(c.nt.Time, c.nt.Valid))
	case propertyGUID:
		if c.ng.Valid {
			c.strs = append(c.strs, c.ng.String())
		} else {
			c.strs = append(c.strs, "")
		}
	default:
		c.strs = append(c.strs, c.ns.String)
	}
}

func (c *propertyColumn) field() *data.Field {
	switch c.kind {
	case propertyInt:
		return data.NewField(c.name, nil, nonNil(c.ints))
	case propertyFloat:
		return data.NewField(c.name, nil, nonNil(c.floats))
	case propertyBool:
		return data.NewField(c.name, nil, nonNil(c.bools))
	case propertyTime:
		return data.NewField(c.name, nil, nonNil(c.times))
	default:
		return data.NewField(c.name, nil, nonNil(c.strs))
	}
}

func nullable[T any](v T, valid bool) *T {
	if !valid {
		return nil
	}
	return &v
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
