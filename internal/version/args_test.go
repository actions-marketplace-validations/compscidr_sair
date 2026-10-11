package version

import (
	"bytes"
	"testing"
)

func TestHandleArgs(t *testing.T) {
	Version = "v9.9.9"
	for _, tt := range []struct {
		args             []string
		code             int
		done             bool
		wantOut, wantErr string
	}{
		{nil, 0, false, "", ""},
		{[]string{"--version"}, 0, true, "v9.9.9\n", ""},
		{[]string{"-v"}, 0, true, "v9.9.9\n", ""},
		{[]string{"--help"}, 0, true, "USAGE", ""},
		{[]string{"--verison"}, 2, true, "", "unknown argument: --verison\nUSAGE"},
		{[]string{"start"}, 2, true, "", "unknown argument: start\nUSAGE"},
	} {
		var out, errOut bytes.Buffer
		code, done := handleArgs(tt.args, "USAGE", &out, &errOut)
		if code != tt.code || done != tt.done || out.String() != tt.wantOut || errOut.String() != tt.wantErr {
			t.Errorf("%q: got (%d, %v, %q, %q)", tt.args, code, done, out.String(), errOut.String())
		}
	}
}
