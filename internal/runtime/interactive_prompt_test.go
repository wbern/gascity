package runtime

import "testing"

func TestIsAtInteractivePrompt(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{
			name: "AskUserQuestion selection menu with cursor",
			pane: "Which library should we use?\n" +
				"❯ 1. date-fns\n" +
				"  2. luxon\n" +
				"  3. moment\n",
			want: true,
		},
		{
			name: "MCP trust prompt with Enter to confirm",
			pane: "New MCP server found in this project: foo\n\n" +
				"❯ 1. Use this MCP server\n" +
				"  2. Use this and all future MCP servers in this project\n" +
				"  3. Continue without using this MCP server\n\n" +
				"Enter to confirm · Esc to cancel\n",
			want: true,
		},
		{
			name: "Enter to confirm frame alone",
			pane: "Quick safety check\nDo you trust the files in this folder?\nEnter to confirm · Esc to cancel\n",
			want: true,
		},
		{
			name: "command approval prompt",
			pane: "● Bash(rm -rf /tmp/x)\nThis command requires approval\n",
			want: true,
		},
		{
			name: "approve edits prompt",
			pane: "● Edit(main.go)\nApprove edits?\n",
			want: true,
		},
		{
			name: "boxed menu row (TUI border)",
			pane: "│ ❯ 1. Yes                                     │\n" +
				"│   2. No                                      │\n",
			want: true,
		},
		{
			name: "bare shell prompt is NOT interactive selection",
			pane: "some output\n❯ \n",
			want: false,
		},
		{
			name: "boxed bare prompt is NOT interactive selection",
			pane: "│ ❯                                            │\n",
			want: false,
		},
		{
			name: "ordinary numbered list in prose is NOT a menu",
			pane: "Here is the plan:\n1. do the thing\n2. do the other thing\n",
			want: false,
		},
		{
			name: "normal assistant output",
			pane: "I have finished the task and all tests pass.\n",
			want: false,
		},
		{
			name: "empty pane",
			pane: "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAtInteractivePrompt(tc.pane); got != tc.want {
				t.Fatalf("IsAtInteractivePrompt() = %v, want %v", got, tc.want)
			}
		})
	}
}
