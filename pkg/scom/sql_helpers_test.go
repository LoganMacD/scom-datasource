package scom

import (
	"database/sql"
	"testing"
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
