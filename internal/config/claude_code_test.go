package config

import "testing"

func TestParseConfigBytesClaudeCodeModelListCloaking(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "defaults to enabled cloaking",
			yaml: "port: 8317\n",
			want: false,
		},
		{
			name: "disables model list cloaking",
			yaml: "claude-code:\n  disable-cloaking-model-list: true\n",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.ClaudeCode.DisableCloakingModelList; got != tt.want {
				t.Fatalf("DisableCloakingModelList = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestParseConfigBytesClaudeCodeModelAllowlist(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("claude-code:\n  model-allowlist:\n    - claude-fable-5\n    - gpt-5.6-luna\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	want := []string{"claude-fable-5", "gpt-5.6-luna"}
	if len(cfg.ClaudeCode.ModelAllowlist) != len(want) {
		t.Fatalf("ModelAllowlist length = %d, want %d", len(cfg.ClaudeCode.ModelAllowlist), len(want))
	}
	for i, modelID := range want {
		if got := cfg.ClaudeCode.ModelAllowlist[i]; got != modelID {
			t.Fatalf("ModelAllowlist[%d] = %q, want %q", i, got, modelID)
		}
	}
}
