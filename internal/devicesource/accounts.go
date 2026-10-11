package devicesource

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"time"
)

// accountRefresh is how long a device's account types are reused before
// dumpsys runs again; device info is read every few seconds.
const accountRefresh = time.Minute

type accountsEntry struct {
	types []string
	at    time.Time
}

// accountTypeRe matches the account lines of `dumpsys account`:
//
//	Account {name=someone@example.com, type=com.google}
var accountTypeRe = regexp.MustCompile(`(?m)^\s*Account \{name=.*, type=([^,}\s]+)\}\s*$`)

// accountTypes returns the distinct, sorted types of the accounts signed in on
// a device. Never the names: those are email addresses and phone numbers. A
// failed read keeps the last answer (sair#77).
func (s *Server) accountTypes(serial string) []string {
	s.accountsMu.Lock()
	e, ok := s.accounts[serial]
	s.accountsMu.Unlock()
	if ok && time.Since(e.at) < accountRefresh {
		return e.types
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := s.shell(ctx, serial, "dumpsys account")
	if err != nil {
		slog.Debug("reading accounts failed", "serial", serial, "error", err)
		return e.types
	}
	types := parseAccountTypes(out)

	s.accountsMu.Lock()
	if s.accounts == nil {
		s.accounts = make(map[string]accountsEntry)
	}
	s.accounts[serial] = accountsEntry{types: types, at: time.Now()}
	s.accountsMu.Unlock()
	return types
}

func parseAccountTypes(out string) []string {
	var types []string
	for _, m := range accountTypeRe.FindAllStringSubmatch(out, -1) {
		types = append(types, m[1])
	}
	slices.Sort(types)
	return slices.Compact(types)
}
