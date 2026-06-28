package breakdown

import (
	"reflect"
	"testing"
)

func TestParseJSONList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "clean array",
			in:   `["Email the landlord","Find boxes","Pack books"]`,
			want: []string{"Email the landlord", "Find boxes", "Pack books"},
		},
		{
			name: "fenced markdown",
			in:   "```json\n[\"Step A\",\"Step B\"]\n```",
			want: []string{"Step A", "Step B"},
		},
		{
			name: "prose around array",
			in:   `Sure! Here you go: ["One","Two","Three"] hope that helps!`,
			want: []string{"One", "Two", "Three"},
		},
		{
			name: "drops empties",
			in:   `["", "Real", "   ", "Tasks"]`,
			want: []string{"Real", "Tasks"},
		},
		{
			name: "garbage returns error",
			in:   "i don't understand",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseJSONList(c.in)
			if c.want == nil {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
