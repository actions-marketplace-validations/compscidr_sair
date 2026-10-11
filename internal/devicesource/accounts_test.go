package devicesource

import (
	"reflect"
	"testing"
)

// Trimmed from a real `dumpsys account` (Android 14), names changed.
const dumpsysAccount = `User UserInfo{0:Owner:c13}:
  Accounts: 3
    Account {name=someone@gmail.com, type=com.google}
    Account {name=+15550100, type=com.google.android.apps.tachyon}
    Account {name=other@gmail.com, type=com.google}

  AccountId, Action_Type, timestamp, UID, TableName, Key
    tmp_id=1, action_type=action_account_add, timestamp=2026-01-01 00:00:00, uid=1000, table=accounts
`

func TestAccountTypesAreDistinctSortedAndNameless(t *testing.T) {
	s := &Server{}
	cmds := fakeShell(s, map[string]string{"dumpsys account": dumpsysAccount})

	want := []string{"com.google", "com.google.android.apps.tachyon"}
	if got := s.accountTypes("SER"); !reflect.DeepEqual(got, want) {
		t.Errorf("types = %q, want %q", got, want)
	}
	// Cached: the next device poll does not run dumpsys again.
	s.accountTypes("SER")
	if len(*cmds) != 1 {
		t.Errorf("dumpsys ran %d times, want 1", len(*cmds))
	}

	if got := parseAccountTypes("  Accounts: 0\n"); got != nil {
		t.Errorf("no accounts: got %q", got)
	}
}

func TestAccountTypesKeepLastAnswerWhenDumpsysFails(t *testing.T) {
	s := &Server{accounts: map[string]accountsEntry{"SER": {types: []string{"com.google"}}}} // stale entry
	fakeShell(s, nil, "dumpsys")
	if got := s.accountTypes("SER"); !reflect.DeepEqual(got, []string{"com.google"}) {
		t.Errorf("types = %q, want the last answer", got)
	}
}
