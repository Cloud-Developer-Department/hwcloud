package feishu

import "testing"

func TestStripLeadingMentions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single mention + slash command", "@_user_1 /mode", "/mode"},
		{"single mention + slash command with args", "@_user_1 /mode auto", "/mode auto"},
		{"single mention + plain text", "@_user_1 hello world", "hello world"},
		{"multiple mentions", "@_user_1 @_user_2 hi", "hi"},
		{"inline mention not stripped", "hello @_user_1 world", "hello @_user_1 world"},
		{"mention only", "@_user_1", ""},
		{"mention only with trailing space", "@_user_1 ", ""},
		{"no mention", "/mode", "/mode"},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripLeadingMentions(c.in)
			if got != c.want {
				t.Errorf("stripLeadingMentions(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
