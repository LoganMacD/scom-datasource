package scom

import (
	"context"
	"database/sql"
	"fmt"
)

// classesQueryByName lists non-abstract SCOM classes (ManagedTypes) whose
// technical name contains the search text (e.g. "Microsoft.Windows.Computer")
// anywhere, not just as a prefix — see searchLikePattern.
var classesQueryByName = fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(64), ManagedTypeId) AS Id,
	TypeName
FROM dbo.ManagedType
WHERE IsAbstract = 0
	AND TypeName LIKE @search ESCAPE '\'
ORDER BY TypeName`, defaultSearchLimit)

// classesQueryByDisplayName searches classes by their localized display name
// (e.g. "Agent") instead of the technical TypeName, via dbo.ManagedTypeView —
// SCOM's own view joining ManagedType to LocalizedText on
// LTStringId = ManagedTypeId (LTStringType 1 = display name), which is the
// canonical join; LocalizedText.ElementName is not it. LanguageCode is
// pinned to 'ENU' — SCOM's base install language, always present alongside
// any additional language packs. If this deployment's operators primarily
// use another language, this filter will need to match that LanguageCode
// instead.
var classesQueryByDisplayName = fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(64), Id) AS Id,
	DisplayName
FROM dbo.ManagedTypeView
WHERE Abstract = 0
	AND LanguageCode = 'ENU'
	AND DisplayName LIKE @search ESCAPE '\'
ORDER BY DisplayName`, defaultSearchLimit)

// SearchBy selects which class field SearchClasses matches the search text
// against.
type SearchBy string

const (
	SearchByName        SearchBy = "name"
	SearchByDisplayName SearchBy = "displayName"
)

// classesQueryFor picks which of the two static class-search queries to run.
// Split out from SearchClasses so the SearchBy->query mapping is directly
// testable without a live DB — see classes_test.go.
func classesQueryFor(by SearchBy) string {
	if by == SearchByDisplayName {
		return classesQueryByDisplayName
	}
	return classesQueryByName
}

// SearchClasses backs the /classes resource endpoint used by the query
// editor's class picker.
func SearchClasses(ctx context.Context, db *sql.DB, search string, by SearchBy) ([]Option, error) {
	rows, err := db.QueryContext(ctx, classesQueryFor(by), sql.Named("search", searchLikePattern(search)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Option
	for rows.Next() {
		var opt Option
		if err := rows.Scan(&opt.Value, &opt.Label); err != nil {
			return nil, err
		}
		out = append(out, opt)
	}
	return out, rows.Err()
}
