package scom

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/data"
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

func resolveTypeName(ctx context.Context, db *sql.DB, classID string) (string, error) {
	var typeName string
	err := db.QueryRowContext(ctx,
		`SELECT TypeName FROM dbo.ManagedType WHERE CONVERT(varchar(64), ManagedTypeId) = @classId`,
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

// ListPropertyNames backs the /properties resource endpoint used by the
// query editor's property picker.
func ListPropertyNames(ctx context.Context, db *sql.DB, classID string) ([]Option, error) {
	typeName, err := resolveTypeName(ctx, db, classID)
	if err != nil {
		return nil, err
	}

	viewName, err := resolveClassView(ctx, db, typeName)
	if err != nil {
		return nil, err
	}

	var cols []string
	if viewName != "" {
		cols, err = classViewColumns(ctx, db, viewName)
		if err != nil {
			return nil, fmt.Errorf("list properties for %s: %w", typeName, err)
		}
	}

	// Always-present base fields, not tied to the per-class view.
	out := []Option{
		{Value: "DisplayName", Label: "DisplayName"},
		{Value: "FullName", Label: "FullName"},
		{Value: "Path", Label: "Path"},
		{Value: "IsManaged", Label: "IsManaged"},
	}
	for _, c := range cols {
		out = append(out, Option{Value: c, Label: c})
	}
	return out, nil
}

var baseEntityColumns = map[string]bool{
	"DisplayName": true, "FullName": true, "Path": true, "IsManaged": true,
}

// QueryProperties returns one frame per class among the selected instances,
// with one row per instance and one field per requested property. Instances
// are grouped by class because typed properties live in per-class views.
func QueryProperties(ctx context.Context, db *sql.DB, instanceIDs []string, propertyNames []string) ([]*data.Frame, error) {
	if len(instanceIDs) == 0 {
		return nil, nil
	}

	groups, err := groupInstancesByClass(ctx, db, instanceIDs)
	if err != nil {
		return nil, err
	}

	var frames []*data.Frame
	for typeName, ids := range groups {
		frame, err := queryPropertiesForClass(ctx, db, typeName, ids, propertyNames)
		if err != nil {
			return nil, fmt.Errorf("properties for class %s: %w", typeName, err)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func groupInstancesByClass(ctx context.Context, db *sql.DB, instanceIDs []string) (map[string][]string, error) {
	inSQL, args := inClause("inst", instanceIDs)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
SELECT CONVERT(varchar(64), bme.BaseManagedEntityId), mt.TypeName
FROM dbo.BaseManagedEntity bme
INNER JOIN dbo.ManagedType mt ON bme.BaseManagedTypeId = mt.ManagedTypeId
WHERE bme.BaseManagedEntityId IN %s`, inSQL), args...)
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

func queryPropertiesForClass(ctx context.Context, db *sql.DB, typeName string, instanceIDs []string, requested []string) (*data.Frame, error) {
	viewName, err := resolveClassView(ctx, db, typeName)
	if err != nil {
		return nil, err
	}

	var available []string
	if viewName != "" {
		available, err = classViewColumns(ctx, db, viewName)
		if err != nil {
			return nil, err
		}
	}
	availableSet := map[string]bool{}
	for _, c := range available {
		availableSet[c] = true
	}

	// Only select property names that are either a known base field or an
	// introspected column of this class's view — never interpolate the raw
	// requested name straight into SQL.
	var selected []string
	for _, name := range requested {
		if baseEntityColumns[name] || availableSet[name] {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		selected = append([]string{"DisplayName"}, available...)
	}

	needsClassView := false
	for _, name := range selected {
		if !baseEntityColumns[name] {
			needsClassView = true
			break
		}
	}

	selectCols := make([]string, len(selected))
	for i, name := range selected {
		if baseEntityColumns[name] {
			selectCols[i] = "bme." + quoteIdent(name)
		} else {
			selectCols[i] = "cv." + quoteIdent(name)
		}
	}

	inSQL, args := inClause("inst", instanceIDs)
	query := fmt.Sprintf(`
SELECT bme.DisplayName AS InstanceDisplayName, %s
FROM dbo.BaseManagedEntity bme`, strings.Join(selectCols, ", "))
	if needsClassView {
		query += fmt.Sprintf(`
INNER JOIN dbo.%s cv ON cv.BaseManagedEntityId = bme.BaseManagedEntityId`, quoteIdent(viewName))
	}
	query += fmt.Sprintf(`
WHERE bme.BaseManagedEntityId IN %s`, inSQL)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	values := make([][]string, len(cols))
	for rows.Next() {
		scanArgs := make([]any, len(cols))
		raw := make([]sql.NullString, len(cols))
		for i := range raw {
			scanArgs[i] = &raw[i]
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, err
		}
		for i, v := range raw {
			values[i] = append(values[i], v.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frame := data.NewFrame(typeName)
	for i, col := range cols {
		frame.Fields = append(frame.Fields, data.NewField(col, nil, values[i]))
	}
	return frame, nil
}
