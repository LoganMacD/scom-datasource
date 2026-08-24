package scom

import (
	"database/sql"
	"strings"
	"testing"
)

func TestScopeJoins(t *testing.T) {
	t.Run("neither class nor group", func(t *testing.T) {
		joins, args := scopeJoins("", "")
		if joins != "" || args != nil {
			t.Fatalf("got joins=%q args=%v, want empty", joins, args)
		}
	})

	t.Run("class only", func(t *testing.T) {
		joins, args := scopeJoins("class-1", "")
		if !strings.Contains(joins, "dbo.DerivedManagedTypes dmt") {
			t.Errorf("joins missing DerivedManagedTypes join: %s", joins)
		}
		if !strings.Contains(joins, "dmt.BaseTypeId = @classId") {
			t.Errorf("joins missing classId filter: %s", joins)
		}
		if strings.Contains(joins, "dbo.Relationship") {
			t.Errorf("joins should not include group join when groupID is empty: %s", joins)
		}
		if len(args) != 1 || args[0].(sql.NamedArg).Value != "class-1" {
			t.Errorf("got args=%v, want single classId named arg", args)
		}
	})

	t.Run("group only", func(t *testing.T) {
		joins, args := scopeJoins("", "group-1")
		if !strings.Contains(joins, "dbo.Relationship rel") {
			t.Errorf("joins missing Relationship join: %s", joins)
		}
		if !strings.Contains(joins, "rel.SourceEntityId = @groupId") {
			t.Errorf("joins missing groupId filter: %s", joins)
		}
		if !strings.Contains(joins, "rel.IsDeleted = 0") {
			t.Errorf("joins missing IsDeleted filter on relationship: %s", joins)
		}
		if len(args) != 1 || args[0].(sql.NamedArg).Value != "group-1" {
			t.Errorf("got args=%v, want single groupId named arg", args)
		}
	})

	t.Run("class and group, args in class-then-group order", func(t *testing.T) {
		joins, args := scopeJoins("class-1", "group-1")
		if !strings.Contains(joins, "dbo.DerivedManagedTypes") || !strings.Contains(joins, "dbo.Relationship") {
			t.Errorf("joins missing one of the two scoping joins: %s", joins)
		}
		if len(args) != 2 {
			t.Fatalf("got %d args, want 2", len(args))
		}
		if args[0].(sql.NamedArg).Name != "classId" || args[1].(sql.NamedArg).Name != "groupId" {
			t.Errorf("got args in wrong order: %v", args)
		}
	})
}
