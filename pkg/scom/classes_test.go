package scom

import (
	"fmt"
	"strings"
	"testing"
)

// TestClassListQuery pins the table and columns the class list reads — the
// kind of assertion that would have caught classes.go referencing a column
// that doesn't exist on that table, the same class of bug fixed in health.go.
func TestClassListQuery(t *testing.T) {
	for _, want := range []string{
		"FROM dbo.ManagedType mt",
		"CONVERT(varchar(64), mt.ManagedTypeId) AS Id",
		"mt.TypeName",
		"WHERE mt.IsAbstract = 0",
		"WHERE lt.LTStringId = mt.ManagedTypeId AND lt.LTStringType = 1",
		"ISNULL(disp.LTValue, mt.TypeName) AS DisplayName",
		"ORDER BY DisplayName, mt.TypeName",
	} {
		if !strings.Contains(classListQuery, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, classListQuery)
		}
	}
	if strings.Contains(classListQuery, "%!") {
		t.Errorf("query has a leftover Sprintf verb\nfull query:\n%s", classListQuery)
	}
	// Pinning one LanguageCode is what used to hide classes localized into
	// any other language (this deployment runs ENU and ENA), and classes
	// with no display string at all, whose LanguageCode is NULL and so
	// matches nothing. Language is a preference now — see localizedNameApply.
	if strings.Contains(classListQuery, "LanguageCode = '") {
		t.Errorf("query filters on a single LanguageCode, want a preference order\nfull query:\n%s", classListQuery)
	}
	// The list is searched in memory, so the query itself takes no search
	// text and no cap.
	if strings.Contains(classListQuery, "@search") || strings.Contains(classListQuery, fmt.Sprintf("TOP %d", defaultSearchLimit)) {
		t.Errorf("query should list every class\nfull query:\n%s", classListQuery)
	}
}
