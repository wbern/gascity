package bdshim

import "testing"

func TestParsePRGateCreateArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		want      PRGateCreate
		wantMatch bool
		wantErr   bool
	}{
		{
			name:      "standard",
			args:      []string{"gate", "create", "--blocks", "crm-1", "--type", "gh:pr", "--await-id", "987", "--reason", "wait"},
			want:      PRGateCreate{TargetID: "crm-1", PRNumber: "987"},
			wantMatch: true,
		},
		{
			name:      "equals and reordered",
			args:      []string{"gate", "create", "--await-id=987", "--reason=wait", "--blocks=crm-1", "--type=gh:pr"},
			want:      PRGateCreate{TargetID: "crm-1", PRNumber: "987"},
			wantMatch: true,
		},
		{
			name: "human gate is outside policy",
			args: []string{"gate", "create", "--blocks", "crm-1", "--type", "human", "--reason", "decision"},
		},
		{
			name: "future human flag remains bd responsibility",
			args: []string{"gate", "create", "--blocks", "crm-1", "--type", "human", "--future-flag", "value"},
		},
		{name: "unrelated command", args: []string{"show", "crm-1"}},
		{
			name:    "missing blocks fails closed",
			args:    []string{"gate", "create", "--type", "gh:pr", "--await-id", "987"},
			wantErr: true,
		},
		{
			name:    "missing await id fails closed",
			args:    []string{"gate", "create", "--blocks", "crm-1", "--type", "gh:pr"},
			wantErr: true,
		},
		{
			name:    "non-numeric PR fails closed",
			args:    []string{"gate", "create", "--blocks", "crm-1", "--type", "gh:pr", "--await-id", "abc"},
			wantErr: true,
		},
		{
			name:    "unknown PR-gate flag fails closed",
			args:    []string{"gate", "create", "--blocks", "crm-1", "--type", "gh:pr", "--await-id", "987", "--future-flag", "value"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, match, err := ParsePRGateCreateArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parse error = %v, wantErr %v", err, tt.wantErr)
			}
			if match != tt.wantMatch {
				t.Fatalf("match = %v, want %v", match, tt.wantMatch)
			}
			if got != tt.want {
				t.Fatalf("gate = %#v, want %#v", got, tt.want)
			}
		})
	}
}
