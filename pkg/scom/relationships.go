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

	query, inArgs := expandHostedEntityIDsQuery(ids)

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

func expandHostedEntityIDsQuery(ids []string) (string, []any) {
	inSQL, inArgs := inClause("root", ids)

	// Use iterative frontier expansion in temp tables instead of a recursive
	// CTE: for some customer environments, SQL Server can fail to compile the
	// recursive shape with "query processor ran out of internal resources".
	query := fmt.Sprintf(`
CREATE TABLE #HostingTypes (RelationshipTypeGuid uniqueidentifier PRIMARY KEY);
INSERT INTO #HostingTypes (RelationshipTypeGuid)
SELECT RelationshipTypeGuid
FROM CS.RelationshipType
WHERE HostingInd = 1 OR ContainmentInd = 1;

CREATE TABLE #Visited (Id uniqueidentifier PRIMARY KEY);
CREATE TABLE #Frontier (Id uniqueidentifier PRIMARY KEY);
CREATE TABLE #NextFrontier (Id uniqueidentifier PRIMARY KEY);

INSERT INTO #Frontier (Id)
SELECT bme.BaseManagedEntityId
FROM dbo.BaseManagedEntity bme
WHERE bme.BaseManagedEntityId IN %s
	AND bme.IsDeleted = 0;

INSERT INTO #Visited (Id)
SELECT Id FROM #Frontier;

DECLARE @Depth int = 0;
DECLARE @MaxDepth int = 20;

WHILE @Depth < @MaxDepth AND EXISTS (SELECT 1 FROM #Frontier)
BEGIN
	TRUNCATE TABLE #NextFrontier;

	INSERT INTO #NextFrontier (Id)
	SELECT DISTINCT rel.TargetEntityId
	FROM dbo.Relationship rel
	INNER JOIN #Frontier f ON rel.SourceEntityId = f.Id
	INNER JOIN #HostingTypes ht ON rel.RelationshipTypeId = ht.RelationshipTypeGuid
	WHERE rel.IsDeleted = 0
		AND NOT EXISTS (SELECT 1 FROM #Visited v WHERE v.Id = rel.TargetEntityId);

	IF @@ROWCOUNT = 0 BREAK;

	INSERT INTO #Visited (Id)
	SELECT nf.Id
	FROM #NextFrontier nf;

	TRUNCATE TABLE #Frontier;
	INSERT INTO #Frontier (Id)
	SELECT Id FROM #NextFrontier;

	SET @Depth = @Depth + 1;
END;

SELECT CONVERT(varchar(64), Id) AS Id
FROM #Visited;`, inSQL)

	return query, inArgs
}
