package logging

import "testing"

func TestRedactSecrets(t *testing.T) {
	tests := map[string]string{
		`shell,v2,raw:cmd wifi add-suggestion 'GLaDOS' wpa2 'hunter2'`:     "shell,v2,raw:cmd wifi add-suggestion <redacted>",
		`shell:cmd wifi connect-network "it's here" wpa2 pass word`:        "shell:cmd wifi connect-network <redacted>",
		"cmd wifi add-network bench wpa2 secret":                           "cmd wifi add-network <redacted>",
		"shell,v2,raw:cmd wifi remove-suggestion 'GLaDOS'":                 "shell,v2,raw:cmd wifi remove-suggestion 'GLaDOS'",
		"sair: uninstalled com.example.app (Wi-Fi suggestions removed: x)": "sair: uninstalled com.example.app (Wi-Fi suggestions removed: x)",
	}
	for in, want := range tests {
		if got := RedactSecrets(in); got != want {
			t.Errorf("RedactSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}
