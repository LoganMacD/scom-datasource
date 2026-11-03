package scom

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// classListQuery lists every non-abstract SCOM class (ManagedType) with its
// technical name (TypeName, e.g. "Microsoft.Windows.Computer") and display
// name (e.g. "Windows Computer"). The list is cached and searched in memory
// (see SearchClasses), so one search box can match either name.
//
// The display name comes from dbo.LocalizedText on LTStringId =
// ManagedTypeId with LTStringType 1 (LocalizedText.ElementName is not it),
// resolved by language preference with localizedNameApply rather than by
// pinning one LanguageCode, which used to hide classes localized into any
// other language or carrying no display string at all. A class with no
// display string falls back to its TypeName.
//
// Every non-abstract class is listed, as the old by-name search did; the
// ManagementPack ContentReadable filter only ever applied to the
// display-name search.
var classListQuery = fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), mt.ManagedTypeId) AS Id,
	mt.TypeName,
	%s AS DisplayName
FROM dbo.ManagedType mt%s
WHERE mt.IsAbstract = 0
ORDER BY DisplayName, mt.TypeName`,
	classDisplayName,
	localizedNameApply("mt.ManagedTypeId", "mt.TypeName", "disp"))

// classDisplayName is the display name expression classListQuery selects.
const classDisplayName = "ISNULL(disp.LTValue, mt.TypeName)"

// loadClassOptions runs classListQuery. Each option is labelled with the
// display name and, when it differs, carries the technical name as its
// description, which also tells apart classes that share a display name.
func loadClassOptions(ctx context.Context, db *sql.DB) ([]Option, error) {
	rows, err := db.QueryContext(ctx, classListQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Option{}
	for rows.Next() {
		var id, typeName, displayName string
		if err := rows.Scan(&id, &typeName, &displayName); err != nil {
			return nil, err
		}
		opt := Option{Value: id, Label: displayName}
		if !strings.EqualFold(typeName, displayName) {
			opt.Description = typeName
		}
		out = append(out, opt)
	}
	return out, rows.Err()
}

// ClassOptions returns every non-abstract class, cached for
// metadataCacheTTL.
func (db *DB) ClassOptions(ctx context.Context) ([]Option, error) {
	return db.cache().options.get(ctx, "classes", metadataCacheTTL, func(ctx context.Context) ([]Option, error) {
		return loadClassOptions(ctx, db.Operational)
	})
}

// SearchClasses backs the /classes resource endpoint used by the query
// editor's class picker. It matches the search text against both the
// display name and the technical name (see searchOptions).
func SearchClasses(ctx context.Context, db *DB, search string) ([]Option, error) {
	classes, err := db.ClassOptions(ctx)
	if err != nil {
		return nil, err
	}
	return searchOptions(classes, search, defaultSearchLimit), nil
}
