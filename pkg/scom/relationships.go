package scom

import (
	"context"
	"database/sql"
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
	query, args := expandHostedEntityIDsQuery(ids)
	return queryStrings(ctx, db, query, args)
}

// ExpandScopeIDs resolves a class and/or group to its instances and widens
// them to everything they host or contain, in one batch. It returns the same
// ids as ResolveInstanceIDs followed by ExpandHostedEntityIDs, without
// sending the (possibly large) instance list to the plugin and back.
func ExpandScopeIDs(ctx context.Context, db *sql.DB, classID, groupID string) ([]string, error) {
	if classID == "" && groupID == "" {
		return nil, nil
	}
	query, args := expandScopeIDsQuery(classID, groupID)
	return queryStrings(ctx, db, query, args)
}

func expandHostedEntityIDsQuery(ids []string) (string, []any) {
	// The seed ids are themselves a class/group's raw, unexpanded
	// ResolveInstanceIDs output — every downstream caller of
	// ExpandHostedEntityIDs (counters/performance/alerts scoping) widens from
	// here, so this is the earliest point a large selection can blow past a
	// literal IN list. Seed via a temp table join instead — see
	// idScopeTempTable. #RootIds is always created, even with no rows, so the
	// join below stays valid when ids is empty.
	rootSetupSQL, rootArgs := idScopeTempTable("#RootIds", ids)
	if rootSetupSQL == "" {
		rootSetupSQL = "CREATE TABLE #RootIds (Id uniqueidentifier PRIMARY KEY);\n"
	}
	return rootSetupSQL + hostingExpansionSQL, rootArgs
}

// expandScopeIDsQuery seeds #RootIds straight from the class/group scope
// (see scopeJoins) instead of from a bound id list. DISTINCT because a group
// can relate to the same member through more than one relationship.
func expandScopeIDsQuery(classID, groupID string) (string, []any) {
	joins, args := scopeJoins(classID, groupID)
	seed := `
CREATE TABLE #RootIds (Id uniqueidentifier PRIMARY KEY);
INSERT INTO #RootIds (Id)
SELECT DISTINCT bme.BaseManagedEntityId
FROM dbo.BaseManagedEntity bme` + joins + `
WHERE bme.IsDeleted = 0;
`
	return seed + hostingExpansionSQL, args
}

// hostingExpansionSQL widens #RootIds (which the caller creates and fills)
// to everything it hosts or contains, and selects the result.
//
// It uses iterative frontier expansion in temp tables instead of a recursive
// CTE: for some customer environments, SQL Server can fail to compile the
// recursive shape with "query processor ran out of internal resources".
const hostingExpansionSQL = `
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
INNER JOIN #RootIds r ON r.Id = bme.BaseManagedEntityId
WHERE bme.IsDeleted = 0;

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
FROM #Visited;`

// queryStrings runs a query whose result is a single string column.
func queryStrings(ctx context.Context, db *sql.DB, query string, args []any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
