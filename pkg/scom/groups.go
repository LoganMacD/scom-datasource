package scom

import (
	"context"
)

// groupListQuery lists instances of Microsoft.SystemCenter.InstanceGroup and
// its subclasses (computer groups, custom groups, etc.) via
// dbo.DerivedManagedTypes, which expands the SCOM class hierarchy (a class is
// trivially its own "derived type", so this also matches instances of
// InstanceGroup itself). The list is cached and searched in memory (see
// SearchGroups).
const groupListQuery = `
SELECT
	CONVERT(varchar(64), bme.BaseManagedEntityId) AS Id,
	bme.DisplayName
FROM dbo.BaseManagedEntity bme
INNER JOIN dbo.DerivedManagedTypes dmt ON bme.BaseManagedTypeId = dmt.DerivedTypeId
INNER JOIN dbo.ManagedType mt ON dmt.BaseTypeId = mt.ManagedTypeId
WHERE mt.TypeName = 'Microsoft.SystemCenter.InstanceGroup'
	AND bme.IsDeleted = 0
ORDER BY bme.DisplayName`

// GroupOptions returns every group, cached for metadataCacheTTL.
func (db *DB) GroupOptions(ctx context.Context) ([]Option, error) {
	return db.cache().options.get(ctx, "groups", metadataCacheTTL, func(ctx context.Context) ([]Option, error) {
		return runOptionsQuery(ctx, db.Operational, groupListQuery, nil)
	})
}

// SearchGroups backs the /groups resource endpoint used by the query
// editor's group picker.
func SearchGroups(ctx context.Context, db *DB, search string) ([]Option, error) {
	groups, err := db.GroupOptions(ctx)
	if err != nil {
		return nil, err
	}
	return searchOptions(groups, search, defaultSearchLimit), nil
}
