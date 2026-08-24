package scom

import (
	"fmt"
	"strings"
	"testing"
)

func TestGroupsQuery(t *testing.T) {
	wantContain := []string{
		"FROM dbo.BaseManagedEntity bme",
		"INNER JOIN dbo.DerivedManagedTypes dmt ON bme.BaseManagedTypeId = dmt.DerivedTypeId",
		"INNER JOIN dbo.ManagedType mt ON dmt.BaseTypeId = mt.ManagedTypeId",
		"mt.TypeName = 'Microsoft.SystemCenter.InstanceGroup'",
		"bme.IsDeleted = 0",
		"bme.DisplayName LIKE @search",
		fmt.Sprintf("TOP %d", defaultSearchLimit),
	}
	for _, want := range wantContain {
		if !strings.Contains(groupsQuery, want) {
			t.Errorf("groupsQuery missing %q\nfull query:\n%s", want, groupsQuery)
		}
	}
	if strings.Contains(groupsQuery, "%!") {
		t.Errorf("groupsQuery has a leftover Sprintf verb, want none\nfull query:\n%s", groupsQuery)
	}
}
