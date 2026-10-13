package logging

import "regexp"

// secretCommand matches the Wi-Fi shell subcommands that take a passphrase
// on the command line. Everything after the subcommand goes: the SSID is
// cheap to lose and the quoting varies too much to pick the passphrase out.
var secretCommand = regexp.MustCompile(`(add-suggestion|connect-network|add-network)\b.*`)

// RedactSecrets returns s with any passphrase-bearing Wi-Fi command cut
// after its subcommand, for log lines and lock log entries (sair#100).
func RedactSecrets(s string) string {
	return secretCommand.ReplaceAllString(s, "$1 <redacted>")
}
