package scom

import (
	"context"
	"database/sql"
	"fmt"
)

// ExpandHostedEntityIDs returns ids plus every entity recursively hosted or
// contained by them. Performance counters are almost never collected against
// a top-level instance itself (e.g. a Windows Computer) but against the
// objects it hosts (logical disks, SQL DB engines, processes, ...), so a
// counter search scoped to a chosen instance must widen to its hosted/
// contained descendants or it comes back empty. CS.RelationshipType's
// HostingInd/ContainmentInd flags already account for management-pack-defined
// subtypes of the built-in System.Hosting/System.Containment relationships,
// so no separate type-hierarchy walk is needed.
func ExpandHostedEntityIDs(ctx context.Context, db *sql.DB, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// The recursive CTE below is deliberately written to keep Id as
	// uniqueidentifier throughout (no varchar round-trip) and to resolve
	// the Hosting/Containment relationship types once into a table
	// variable up front. Doing the type filter as a join with an OR
	// predicate inside the recursive member, or converting Id to varchar
	// and back on every recursion step, defeats index seeks and can make
	// SQL Server's optimizer give up with "the query processor ran out of
	// internal resources" even at a low MAXRECURSION.
	inSQL, inArgs := inClause("root", ids)
	query := fmt.Sprintf(`
DECLARE @HostingTypes TABLE (RelationshipTypeGuid uniqueidentifier PRIMARY KEY);
INSERT INTO @HostingTypes (RelationshipTypeGuid)
SELECT RelationshipTypeGuid
FROM CS.RelationshipType
WHERE HostingInd = 1 OR ContainmentInd = 1;

;WITH Hosted (Id) AS (
	SELECT bme.BaseManagedEntityId
	FROM dbo.BaseManagedEntity bme
	WHERE bme.BaseManagedEntityId IN %s
		AND bme.IsDeleted = 0

	UNION ALL

	SELECT rel.TargetEntityId
	FROM dbo.Relationship rel
	INNER JOIN Hosted h ON rel.SourceEntityId = h.Id
	WHERE rel.IsDeleted = 0
		AND rel.RelationshipTypeId IN (SELECT RelationshipTypeGuid FROM @HostingTypes)
)
SELECT DISTINCT CONVERT(varchar(64), Id) AS Id FROM Hosted
OPTION (MAXRECURSION 20)`, inSQL)

	rows, err := db.QueryContext(ctx, query, inArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
