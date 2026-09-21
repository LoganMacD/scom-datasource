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
// (e.g. "Agent") instead of the technical TypeName. The canonical join for
// that name is dbo.LocalizedText on LTStringId = ManagedTypeId with
// LTStringType 1; LocalizedText.ElementName is not it.
//
// This used to go through dbo.ManagedTypeView with LanguageCode pinned to
// 'ENU'. The view adds only a ManagementPack ContentReadable filter
// (replicated below) and that same LocalizedText join, unrestricted — so it
// yields one row per installed language pack, and pinning the language was
// how this query collapsed them. The cost was that a class localized into
// another language, or carrying no display string at all, was silently
// unsearchable by name. localizedNameApply picks a name by preference
// instead, falling back to the technical TypeName so every non-abstract
// class is findable here.
var classesQueryByDisplayName = fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(64), mt.ManagedTypeId) AS Id,
	%s AS DisplayName
FROM dbo.ManagedType mt
INNER JOIN dbo.ManagementPack mp ON mp.ManagementPackId = mt.ManagementPackId
	AND mp.ContentReadable = 1%s
WHERE mt.IsAbstract = 0
	AND %s LIKE @search ESCAPE '\'
ORDER BY %s`,
	defaultSearchLimit,
	classDisplayName,
	localizedNameApply("mt.ManagedTypeId", "disp"),
	classDisplayName,
	classDisplayName)

// classDisplayName is the display name expression classesQueryByDisplayName
// selects, filters and orders by — spelled once so the three can't drift
// apart (a SELECT alias can't be reused in a WHERE clause).
const classDisplayName = "ISNULL(disp.LTValue, mt.TypeName)"

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
