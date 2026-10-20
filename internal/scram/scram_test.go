package scram

import (
	"encoding/base64"
	"errors"
	"testing"
)

const (
	vSalt        = "W22ZaJ0SNY7soEsUEjb6gQ=="
	vServerNonce = "%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	vServerFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	vClientFinal = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	vServerFinal = "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
)

func salt(t *testing.T) []byte {
	b, err := base64.StdEncoding.DecodeString(vSalt)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRFC7677Vector(t *testing.T) {
	c := NewClient("user", "pencil", "rOprNGfwEbeRWgbNEkqO")
	if got := c.First(); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("First = %q", got)
	}
	final, err := c.Final(vServerFirst)
	if err != nil || final != vClientFinal {
		t.Fatalf("Final = %q, %v", final, err)
	}
	if err := c.Verify(vServerFinal); err != nil {
		t.Fatal(err)
	}

	s := salt(t)
	srv, user, err := StartServer(c.First(), s, 4096, vServerNonce)
	if err != nil || user != "user" {
		t.Fatalf("StartServer = %q, %v", user, err)
	}
	if srv.ServerFirst != vServerFirst {
		t.Fatalf("ServerFirst = %q", srv.ServerFirst)
	}
	stored, server := Keys(SaltedPassword("pencil", s, 4096))
	sf, err := srv.Finish(vClientFinal, stored, server)
	if err != nil || sf != vServerFinal {
		t.Fatalf("Finish = %q, %v", sf, err)
	}
}

func TestFinalWithSalted(t *testing.T) {
	c := NewClient("user", "", "rOprNGfwEbeRWgbNEkqO")
	final, err := c.FinalWithSalted(vServerFirst, SaltedPassword("pencil", salt(t), 4096))
	if err != nil || final != vClientFinal {
		t.Fatalf("FinalWithSalted = %q, %v", final, err)
	}
}

func TestWrongPassword(t *testing.T) {
	c := NewClient("user", "pencil2", "rOprNGfwEbeRWgbNEkqO")
	srv, _, err := StartServer(c.First(), salt(t), 4096, vServerNonce)
	if err != nil {
		t.Fatal(err)
	}
	final, err := c.Final(srv.ServerFirst)
	if err != nil {
		t.Fatal(err)
	}
	stored, server := Keys(SaltedPassword("pencil", salt(t), 4096))
	if _, err := srv.Finish(final, stored, server); !errors.Is(err, ErrProof) {
		t.Fatalf("err = %v, want ErrProof", err)
	}
}

func TestRejectChannelBinding(t *testing.T) {
	for _, cf := range []string{"p=tls-unique,,n=user,r=x", "y,,n=user,r=x", "n,a=x,n=user,r=x", "n,,n=user", "n,,r=x", "n,,n=a=b,r=x", "n,,n=user,r=x,e=1"} {
		if _, _, err := StartServer(cf, salt(t), 4096, "s"); !errors.Is(err, ErrFormat) {
			t.Errorf("%q: err = %v, want ErrFormat", cf, err)
		}
	}
}

func TestTamperedNonce(t *testing.T) {
	srv, _, err := StartServer("n,,n=user,r=rOprNGfwEbeRWgbNEkqO", salt(t), 4096, vServerNonce)
	if err != nil {
		t.Fatal(err)
	}
	stored, server := Keys(SaltedPassword("pencil", salt(t), 4096))
	bad := "c=biws,r=rOprNGfwEbeRWgbNEkqOxxx,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if _, err := srv.Finish(bad, stored, server); !errors.Is(err, ErrProof) && !errors.Is(err, ErrFormat) {
		t.Fatalf("err = %v", err)
	}
	bad = "c=eSws,r=rOprNGfwEbeRWgbNEkqO" + vServerNonce + ",p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if _, err := srv.Finish(bad, stored, server); !errors.Is(err, ErrFormat) {
		t.Fatalf("bad c=: err = %v", err)
	}
}

func TestClientIterationBounds(t *testing.T) {
	for _, i := range []string{"1", "4095", "10000001", "+4096", "-1", " 4096", "2000000000"} {
		c := NewClient("user", "pencil", "abc")
		if _, err := c.Final("r=abcd,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=" + i); !errors.Is(err, ErrFormat) {
			t.Errorf("i=%q: err = %v, want ErrFormat", i, err)
		}
	}
	c := NewClient("a,b", "pencil", "abc")
	if _, err := c.Final("r=abcd,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"); !errors.Is(err, ErrFormat) {
		t.Errorf("bad username: err = %v", err)
	}
}

func TestClientRejectsBadServerFirst(t *testing.T) {
	c := NewClient("user", "pencil", "abc")
	for _, sf := range []string{"r=zzz,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096", "r=abcd,s=W22ZaJ0SNY7soEsUEjb6gQ==", "r=abcd,s=!!,i=4096", "r=abcd,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=x", "r=abcd,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=0"} {
		if _, err := c.Final(sf); err == nil {
			t.Errorf("%q accepted", sf)
		}
	}
	if err := c.Verify("v=AAAA"); err == nil {
		t.Error("Verify before Final accepted")
	}
}
