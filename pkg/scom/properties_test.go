package scom

import (
	"strings"
	"testing"
)

func TestBuildPropertiesQuery(t *testing.T) {
	props := classPropertyLists{
		TypeName: "Microsoft.Windows.Computer",
		ViewName: "MTV_Computer",
		Columns:  []string{"NetbiosComputerName", "LogicalProcessors"},
	}

	t.Run("selects only known names, quoted, ordered by instance name", func(t *testing.T) {
		query, args := buildPropertiesQuery(props, []string{"inst-1"}, []string{"LogicalProcessors", "DisplayName", "x]; DROP TABLE y; --"})
		for _, want := range []string{
			"SELECT bme.DisplayName AS InstanceDisplayName, cv.[LogicalProcessors], bme.[DisplayName]",
			"INNER JOIN dbo.[MTV_Computer] cv ON cv.BaseManagedEntityId = bme.BaseManagedEntityId",
			"WHERE bme.BaseManagedEntityId IN (SELECT Id FROM #PropertyScope)",
			"ORDER BY bme.DisplayName",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("query missing %q\nfull query:\n%s", want, query)
			}
		}
		if strings.Contains(query, "DROP TABLE") {
			t.Errorf("an unknown property name reached the SQL\nfull query:\n%s", query)
		}
		if len(args) != 1 {
			t.Errorf("got %d args, want 1", len(args))
		}
	})

	t.Run("base columns only need no class view", func(t *testing.T) {
		query, _ := buildPropertiesQuery(props, []string{"inst-1"}, []string{"FullName"})
		if strings.Contains(query, "MTV_Computer") {
			t.Errorf("query joins the class view for base columns only\nfull query:\n%s", query)
		}
	})

	t.Run("no valid names selects every property", func(t *testing.T) {
		query, _ := buildPropertiesQuery(props, []string{"inst-1"}, nil)
		if !strings.Contains(query, "bme.[DisplayName], cv.[NetbiosComputerName], cv.[LogicalProcessors]") {
			t.Errorf("query should select DisplayName and every class column\nfull query:\n%s", query)
		}
	})
}

func TestPropertyKindOf(t *testing.T) {
	cases := map[string]propertyKind{
		"INT": propertyInt, "bigint": propertyInt, "TINYINT": propertyInt,
		"FLOAT": propertyFloat, "DECIMAL": propertyFloat, "MONEY": propertyFloat,
		"BIT":      propertyBool,
		"DATETIME": propertyTime, "DATETIME2": propertyTime, "DATE": propertyTime,
		"UNIQUEIDENTIFIER": propertyGUID,
		"NVARCHAR":         propertyString, "XML": propertyString, "": propertyString,
	}
	for dbType, want := range cases {
		if got := propertyKindOf(dbType); got != want {
			t.Errorf("propertyKindOf(%q) = %v, want %v", dbType, got, want)
		}
	}
}

func TestPropertyColumnNulls(t *testing.T) {
	c := &propertyColumn{name: "LogicalProcessors", kind: propertyInt}
	c.ni.Int64, c.ni.Valid = 8, true
	c.take()
	c.ni.Valid = false
	c.take()

	f := c.field()
	if f.Len() != 2 {
		t.Fatalf("got %d values, want 2", f.Len())
	}
	if v, ok := f.ConcreteAt(0); !ok || v.(int64) != 8 {
		t.Errorf("first value = %v, want 8", v)
	}
	if _, ok := f.ConcreteAt(1); ok {
		t.Error("NULL scanned as a value, want null")
	}
}
