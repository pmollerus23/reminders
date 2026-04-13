package summarize

// White-box test: same package so validateSummary is accessible.

import "testing"

func TestValidateSummary(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "valid summary",
			input:   "The user prefers morning reminders and wants to exercise daily.",
			wantErr: false,
		},
		{
			name:    "exactly 20 chars",
			input:   "12345678901234567890",
			wantErr: false,
		},
		{
			name:    "too short — empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "too short — 19 chars",
			input:   "1234567890123456789",
			wantErr: true,
		},
		{
			name:    "refusal prefix: i cannot",
			input:   "I cannot summarize this conversation as it contains no facts.",
			wantErr: true,
		},
		{
			name:    "refusal prefix: i'm unable",
			input:   "I'm unable to produce a summary for this content.",
			wantErr: true,
		},
		{
			name:    "refusal prefix: i don't",
			input:   "I don't have enough information to write a summary here.",
			wantErr: true,
		},
		{
			name:    "refusal phrase mid-sentence is allowed",
			input:   "The user mentioned that I cannot is a common refusal phrase used by LLMs.",
			wantErr: false,
		},
		{
			name:    "case insensitive refusal detection",
			input:   "I CANNOT summarize this.",
			wantErr: true,
		},
		{
			name:    "long valid summary",
			input:   "The user is working toward running a 5k every week. They prefer reminders in the morning and have mentioned their partner Alice several times. They drink black coffee and meditate daily.",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSummary(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateSummary(%q) error=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
		})
	}
}
