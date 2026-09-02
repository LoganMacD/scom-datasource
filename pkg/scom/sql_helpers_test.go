package scom

import (
	"database/sql"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestInClause(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		clause, args := inClause("inst", nil)
		if clause != "" || args != nil {
			t.Fatalf("got clause=%q args=%v, want empty", clause, args)
		}
	})

	t.Run("single value", func(t *testing.T) {
		clause, args := inClause("inst", []string{"a"})
		if clause != "(@inst0)" {
			t.Fatalf("got clause=%q, want %q", clause, "(@inst0)")
		}
		if len(args) != 1 {
			t.Fatalf("got %d args, want 1", len(args))
		}
		named, ok := args[0].(sql.NamedArg)
		if !ok {
			t.Fatalf("arg is %T, want sql.NamedArg", args[0])
		}
		if named.Name != "inst0" || named.Value != "a" {
			t.Fatalf("got named=%+v, want Name=inst0 Value=a", named)
		}
	})

	t.Run("multiple values preserve order", func(t *testing.T) {
		clause, args := inClause("ctr", []string{"x", "y", "z"})
		want := "(@ctr0, @ctr1, @ctr2)"
		if clause != want {
			t.Fatalf("got clause=%q, want %q", clause, want)
		}
		if len(args) != 3 {
			t.Fatalf("got %d args, want 3", len(args))
		}
		for i, v := range []string{"x", "y", "z"} {
			named := args[i].(sql.NamedArg)
			if named.Value != v {
				t.Fatalf("arg %d: got value %v, want %v", i, named.Value, v)
			}
		}
	})
}

func TestSearchLikePattern(t *testing.T) {
	cases := []struct {
		name   string
		search string
		want   string
	}{
		{"plain text becomes a contains match", "cpu", "%cpu%"},
		{"empty input matches everything", "", "%%"},
		{"user wildcard * becomes SQL %", "CPU*Time", "%CPU%Time%"},
		{"user wildcard ? becomes SQL _", "Disk ? Read", "%Disk _ Read%"},
		{"literal % is escaped, not a wildcard", "100%", `%100\%%`},
		{"literal _ is escaped, not a wildcard", "a_b", `%a\_b%`},
		{"literal [ is escaped, not a wildcard", "[test]", `%\[test]%`},
		{"literal backslash is escaped", `a\b`, `%a\\b%`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := searchLikePattern(c.search); got != c.want {
				t.Errorf("searchLikePattern(%q) = %q, want %q", c.search, got, c.want)
			}
		})
	}
}

func TestIDScopeTempTable(t *testing.T) {
	t.Run("empty values applies no scope", func(t *testing.T) {
		query, args := idScopeTempTable("#Scope", nil)
		if query != "" || args != nil {
			t.Fatalf("got query=%q args=%v, want empty", query, args)
		}
	})

	t.Run("any size list binds exactly one parameter", func(t *testing.T) {
		many := make([]string, 5000)
		for i := range many {
			many[i] = "11111111-1111-1111-1111-111111111111"
		}

		query, args := idScopeTempTable("#Scope", many)
		if !strings.Contains(query, "CREATE TABLE #Scope (Id uniqueidentifier PRIMARY KEY)") {
			t.Errorf("query missing temp table creation: %s", query)
		}
		if !strings.Contains(query, "FROM STRING_SPLIT(@idScopeValues, ',')") {
			t.Errorf("query missing STRING_SPLIT population: %s", query)
		}
		// The whole point: parameter count must stay at 1 no matter how many
		// ids are scoped — SQL Server hard-caps a single RPC at ~2,100
		// parameters, and that cap applies to the call as a whole, not to any
		// one statement inside a multi-statement batch.
		if len(args) != 1 {
			t.Fatalf("got %d args, want exactly 1 regardless of list length", len(args))
		}
		named, ok := args[0].(sql.NamedArg)
		if !ok {
			t.Fatalf("arg is %T, want sql.NamedArg", args[0])
		}
		if named.Name != "idScopeValues" {
			t.Errorf("got param name %q, want idScopeValues", named.Name)
		}
		// VarCharMax, not a bare string: the value is pure ASCII (GUIDs and
		// comma delimiters), so it's sent as VARCHAR(MAX) rather than paying
		// for NVARCHAR's 2-bytes-per-char Unicode encoding on a large list.
		value, ok := named.Value.(mssql.VarCharMax)
		if !ok {
			t.Fatalf("arg value is %T, want mssql.VarCharMax", named.Value)
		}
		joined := string(value)
		if wantCommas := len(many) - 1; strings.Count(joined, ",") != wantCommas {
			t.Errorf("got %d commas in joined value, want %d", strings.Count(joined, ","), wantCommas)
		}
	})
}
