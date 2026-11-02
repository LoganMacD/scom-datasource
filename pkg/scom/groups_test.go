package scom

import (
	"strings"
	"testing"
)

func TestGroupListQuery(t *testing.T) {
	wantContain := []string{
		"FROM dbo.BaseManagedEntity bme",
		"INNER JOIN dbo.DerivedManagedTypes dmt ON bme.BaseManagedTypeId = dmt.DerivedTypeId",
		"INNER JOIN dbo.ManagedType mt ON dmt.BaseTypeId = mt.ManagedTypeId",
		"mt.TypeName = 'Microsoft.SystemCenter.InstanceGroup'",
		"bme.IsDeleted = 0",
		"ORDER BY bme.DisplayName",
	}
	for _, want := range wantContain {
		if !strings.Contains(groupListQuery, want) {
			t.Errorf("groupListQuery missing %q\nfull query:\n%s", want, groupListQuery)
		}
	}
	// Searched in memory (see SearchGroups): no search text, no cap.
	if strings.Contains(groupListQuery, "@search") || strings.Contains(groupListQuery, "TOP ") {
		t.Errorf("groupListQuery should list every group\nfull query:\n%s", groupListQuery)
	}
}
