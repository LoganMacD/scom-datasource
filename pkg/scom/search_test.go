package scom

import (
	"reflect"
	"testing"
)

// searchMatcher must agree with searchLikePattern, since the same search box
// is answered from SQL for some pickers and from a cached list for others.
func TestSearchMatcher(t *testing.T) {
	cases := []struct {
		search string
		value  string
		want   bool
	}{
		{"cpu", "Processor - % CPU Time", true},
		{"CPU", "processor - % cpu time", true}, // case-insensitive, like SQL Server's default collation
		{"", "anything", true},
		{"CPU*Time", "% CPU Busy Time", true},
		{"Disk ? Read", "Disk 1 Read", true},
		{"Disk ? Read", "Disk 12 Read", false},
		{"100%", "100% busy", true},
		{"100%", "1000 busy", false}, // % is literal, not a wildcard
		{"a_b", "a_b", true},
		{"a_b", "axb", false}, // _ is literal
		{"[test]", "a [test] b", true},
		{`a\b`, `a\b`, true},
		{"memory", "Processor", false},
	}
	for _, c := range cases {
		if got := searchMatcher(c.search)(c.value); got != c.want {
			t.Errorf("searchMatcher(%q)(%q) = %v, want %v", c.search, c.value, got, c.want)
		}
	}
}

func TestSearchOptions(t *testing.T) {
	opts := []Option{
		{Value: "1", Label: "Agent Health", Description: "Microsoft.SystemCenter.AgentHealth"},
		{Value: "2", Label: "Computer"},
		{Value: "3", Label: "Windows Computer", Description: "Microsoft.Windows.Computer"},
		{Value: "4", Label: "Windows Computer Role", Description: "Microsoft.Windows.ComputerRole"},
	}

	t.Run("matches the description too", func(t *testing.T) {
		got := searchOptions(opts, "Microsoft.Windows.Computer", 0)
		if len(got) != 2 || got[0].Value != "3" {
			t.Fatalf("got %+v, want the exact technical-name match first", got)
		}
	})

	t.Run("exact and prefix matches come first", func(t *testing.T) {
		got := searchOptions(opts, "computer", 0)
		var values []string
		for _, o := range got {
			values = append(values, o.Value)
		}
		// "Computer" is exact; the rest only contain the word, in list order.
		if want := []string{"2", "3", "4"}; !reflect.DeepEqual(values, want) {
			t.Errorf("got order %v, want %v", values, want)
		}
	})

	t.Run("caps the result", func(t *testing.T) {
		if got := searchOptions(opts, "", 2); len(got) != 2 {
			t.Errorf("got %d options, want 2", len(got))
		}
	})

	t.Run("does not reorder the cached list", func(t *testing.T) {
		before := append([]Option(nil), opts...)
		_ = searchOptions(opts, "computer", 0)
		if !reflect.DeepEqual(opts, before) {
			t.Error("searchOptions modified its input")
		}
	})
}
