package scom

import (
	"context"
	"database/sql"
	"fmt"
)

// groupsQuery lists instances of Microsoft.SystemCenter.InstanceGroup and its
// subclasses (computer groups, custom groups, etc.) via dbo.DerivedManagedTypes,
// which expands the SCOM class hierarchy (a class is trivially its own "derived
// type", so this also matches instances of InstanceGroup itself).
var groupsQuery = fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(64), bme.BaseManagedEntityId) AS Id,
	bme.DisplayName
FROM dbo.BaseManagedEntity bme
INNER JOIN dbo.DerivedManagedTypes dmt ON bme.BaseManagedTypeId = dmt.DerivedTypeId
INNER JOIN dbo.ManagedType mt ON dmt.BaseTypeId = mt.ManagedTypeId
WHERE mt.TypeName = 'Microsoft.SystemCenter.InstanceGroup'
	AND bme.IsDeleted = 0
	AND bme.DisplayName LIKE @search + '%%'
ORDER BY bme.DisplayName`, defaultSearchLimit)

// SearchGroups backs the /groups resource endpoint used by the query
// editor's group picker.
func SearchGroups(ctx context.Context, db *sql.DB, search string) ([]Option, error) {
	rows, err := db.QueryContext(ctx, groupsQuery, sql.Named("search", search))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
