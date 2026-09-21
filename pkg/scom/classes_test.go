package scom

import (
	"fmt"
	"strings"
	"testing"
)

// TestClassQueries pins the exact table/view and columns each static class
// query hits against — this is the kind of assertion that would have caught
// pkg/scom/classes.go referencing a column that doesn't exist on that table,
// the same class of bug fixed in health.go.
func TestClassQueries(t *testing.T) {
	cases := []struct {
		name        string
		query       string
		wantContain []string
	}{
		{
			name:  "by name hits dbo.ManagedType.TypeName",
			query: classesQueryByName,
			wantContain: []string{
				"FROM dbo.ManagedType",
				"TypeName LIKE @search",
				"WHERE IsAbstract = 0",
				fmt.Sprintf("TOP %d", defaultSearchLimit),
			},
		},
		{
			name:  "by display name resolves LocalizedText by preference",
			query: classesQueryByDisplayName,
			wantContain: []string{
				"FROM dbo.ManagedType mt",
				// The ContentReadable filter dbo.ManagedTypeView used to
				// supply has to be carried over by hand now.
				"AND mp.ContentReadable = 1",
				"WHERE mt.IsAbstract = 0",
				"WHERE lt.LTStringId = mt.ManagedTypeId AND lt.LTStringType = 1",
				"ISNULL(disp.LTValue, mt.TypeName) LIKE @search",
				fmt.Sprintf("TOP %d", defaultSearchLimit),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, want := range c.wantContain {
				if !strings.Contains(c.query, want) {
					t.Errorf("query missing %q\nfull query:\n%s", want, c.query)
				}
			}
			if strings.Contains(c.query, "%!") {
				t.Errorf("query has a leftover Sprintf verb, want none\nfull query:\n%s", c.query)
			}
			// Pinning one LanguageCode is what used to hide classes localized
			// into any other language (this deployment runs ENU and ENA), and
			// classes with no display string at all, whose LanguageCode is
			// NULL and so matches nothing. Language is a preference now — see
			// localizedNameApply.
			if strings.Contains(c.query, "LanguageCode = '") {
				t.Errorf("query filters on a single LanguageCode, want a preference order\nfull query:\n%s", c.query)
			}
		})
	}
}

func TestClassesQueryFor(t *testing.T) {
	cases := []struct {
		by   SearchBy
		want string
	}{
		{SearchByName, classesQueryByName},
		{SearchByDisplayName, classesQueryByDisplayName},
		{"", classesQueryByName}, // unset/unknown SearchBy falls back to name
	}
	for _, c := range cases {
		if got := classesQueryFor(c.by); got != c.want {
			t.Errorf("classesQueryFor(%q): got a different query than expected", c.by)
		}
	}
}
