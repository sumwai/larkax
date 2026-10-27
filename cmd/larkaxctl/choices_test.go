package main

import (
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// TestJoinChoiceNames 直接钉住分隔规则的几种形态。
//
// 这一层被 `--format` 与 `--endpoint` 两处共用，两边各有自己的整句用例，
// 但那只能间接覆盖：两处恰好都只有两项或三项时，一项那一支根本没有调用方
// 会走到。规则放在这里，判据也放在这里。
func TestJoinChoiceNames(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"两项", []string{"table", "json"}, "table 或 json"},
		{"三项", []string{"chat", "messages", "responses"}, "chat、messages 或 responses"},
		{"一项", []string{"only"}, "only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cli.JoinChoiceNames(tc.in); got != tc.want {
				t.Errorf("joinChoiceNames(%q) = %q，想要 %q", tc.in, got, tc.want)
			}
		})
	}
}
