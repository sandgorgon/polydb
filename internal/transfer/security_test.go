package transfer_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sandgorgon/polydb/internal/security"
	"github.com/sandgorgon/polydb/internal/session"
	"github.com/sandgorgon/polydb/internal/transfer"
)

const fp1 = "1111111111111111111111111111111111111111111111111111111111111111"

// grantsText lists grants with kinds by name, sorted.
func grantsText(t *testing.T, s *session.Session) string {
	t.Helper()
	res, err := s.Exec(context.Background(), `show grants`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range res[0].Rows {
		out = append(out, fmt.Sprint(r...))
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}

func TestSecurityExportImport(t *testing.T) {
	ctx := context.Background()
	src := open(t)
	s := session.New(src, security.AdminUser)
	defer s.Close()
	exec(t, s, `create database sales; use sales; define kind Order { n: int };
		create user ann password "ann-pw"; create user bob password "bob-pw";
		alter user bob add key "`+fp1+`" as "laptop";
		create role staff; create role analyst; grant analyst to staff; grant staff to ann;
		grant read on database sales to analyst; grant write on kind sales.Order to staff;
		grant admin on server to bob`, nil)
	var buf bytes.Buffer
	st, err := transfer.ExportSecurity(ctx, src, &buf, transfer.JSONL, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Users != 3 || st.Roles != 2 || st.Grants != 4 {
		t.Fatalf("export stats %+v", st)
	}

	// Into a server with the same database: everything arrives, and
	// ann's password works there.
	dst := open(t)
	d := session.New(dst, security.AdminUser)
	defer d.Close()
	exec(t, d, `create database sales; use sales; define kind Order { n: int }`, nil)
	ist, err := transfer.ImportSecurity(ctx, dst, bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if ist.Users != 3 || ist.Kept != 1 || ist.Grants != 4 || ist.Skipped != 0 { // admin kept
		t.Fatalf("import stats %+v", ist)
	}
	if a, b := grantsText(t, s), grantsText(t, d); a != b {
		t.Fatalf("grants differ:\n%s\n---\n%s", a, b)
	}
	ann, _ := dst.Security().User("ann")
	if ann.Creds == nil || !ann.Creds.Verify("ann-pw") || ann.Creds.Verify("wrong") {
		t.Error("ann's password didn't travel")
	}
	if bob, _ := dst.Security().User("bob"); len(bob.Keys) != 1 || bob.Keys[0].Label != "laptop" {
		t.Errorf("bob's key: %+v", bob.Keys)
	}
	if !dst.Security().Allowed("ann", security.Write, "sales", must(dst.Database("sales")).Catalog().Kinds()[0].ID) {
		t.Error("ann can't write Order through staff")
	}

	// Importing again changes nothing; a changed password is kept
	// unless replace is asked for.
	dst.Security().SetPassword("ann", "new-pw")
	if _, err := transfer.ImportSecurity(ctx, dst, bytes.NewReader(buf.Bytes()), false); err != nil {
		t.Fatal(err)
	}
	if ann, _ := dst.Security().User("ann"); !ann.Creds.Verify("new-pw") {
		t.Error("import replaced a password without replace")
	}
	if _, err := transfer.ImportSecurity(ctx, dst, bytes.NewReader(buf.Bytes()), true); err != nil {
		t.Fatal(err)
	}
	if ann, _ := dst.Security().User("ann"); !ann.Creds.Verify("ann-pw") {
		t.Error("replace didn't restore the password")
	}

	// Without passwords, and into a server lacking the database: users
	// come without passwords, grants on the missing database are skipped.
	buf.Reset()
	if _, err := transfer.ExportSecurity(ctx, src, &buf, transfer.Native, false); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("stored_key")) {
		t.Fatal("an export without passwords holds credentials")
	}
	bare := open(t)
	ist, err = transfer.ImportSecurity(ctx, bare, bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if ist.Skipped != 2 || ist.Grants != 2 {
		t.Errorf("import into a bare server: %+v", ist)
	}
	if ann, _ := bare.Security().User("ann"); ann.Creds != nil {
		t.Error("a user got a password from an export without them")
	}
}
