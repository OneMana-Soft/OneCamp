package ldap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// A directory entry is someone's account only with an address the directory
// holds. One without mail or userPrincipalName became "<typed>@ldap.local",
// an address nobody receives mail at, and the account was made with it.
func TestAnEntryWithoutAnAddressGivesNone(t *testing.T) {
	entry := ldap.NewEntry("uid=sam,ou=people,dc=example,dc=com", map[string][]string{
		"uid": {"sam"},
		"cn":  {"Sam Rivera"},
	})
	got := userFromEntry(entry, "memberOf")
	if got.Email != "" {
		t.Fatalf("an entry with no mail and no userPrincipalName gave the address %q; want none, so sign-in is refused", got.Email)
	}
	if got.DN != "uid=sam,ou=people,dc=example,dc=com" || got.Username != "sam" {
		t.Fatalf("the refusal can't say whose entry it was: %+v", got)
	}
}

func TestAnEntrysAddressIsTheDirectorys(t *testing.T) {
	for name, tc := range map[string]struct {
		attrs map[string][]string
		want  string
	}{
		"mail":                            {map[string][]string{"mail": {"Sam@Example.com"}, "userPrincipalName": {"sam@corp.example.com"}}, "sam@example.com"},
		"userPrincipalName, with no mail": {map[string][]string{"userPrincipalName": {"Sam@Corp.Example.com"}}, "sam@corp.example.com"},
	} {
		attrs := tc.attrs
		attrs["sAMAccountName"] = []string{"sam"}
		attrs["memberOf"] = []string{"CN=OneCamp Admins,OU=Groups,DC=example,DC=com"}
		got := userFromEntry(ldap.NewEntry("CN=Sam,OU=People,DC=example,DC=com", attrs), "memberOf")
		if got.Email != tc.want {
			t.Errorf("%s: address %q, want %q", name, got.Email, tc.want)
		}
		if len(got.Groups) != 1 {
			t.Errorf("%s: groups %v, want the one memberOf names", name, got.Groups)
		}
	}
}

// LDAPS to a directory whose certificate a company's own authority issued.
// The system's authorities don't know it, so the connection was refused and
// there was no way to trust it. With LDAP_CA_CERT naming the authority's PEM
// the connection is made. The stand-in directory completes the TLS handshake
// and hangs up, so the sign-in still fails, at the bind: what matters is
// which side of the handshake it fails on.
func TestLDAPSTrustsTheAuthorityLDAPCACertNames(t *testing.T) {
	caPEM, serverCert := testAuthority(t, "127.0.0.1")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	client := LDAPClient{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, UseTLS: true,
		BindDN: "cn=reader,dc=example,dc=com", BindPassword: "reader-password",
		BaseDN: "dc=example,dc=com", UserFilter: "(uid=%s)",
	}

	_, err = client.Authenticate("sam", "password")
	if err == nil || !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Fatalf("without LDAP_CA_CERT: %v, want the directory's certificate refused", err)
	}

	client.CACertPath = caFile
	_, err = client.Authenticate("sam", "password")
	if err == nil || !strings.HasPrefix(err.Error(), "ldap bind failed") {
		t.Fatalf("with LDAP_CA_CERT naming the authority: %v, want the connection made (and the bind then refused)", err)
	}
}

// A bundle that can't be used says so, naming the setting, rather than
// leaving every sign-in to fail with a certificate error.
func TestAnUnusableLDAPCACertIsNamed(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.der")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "missing.pem"): "no such file",
		notPEM:                            "holds no PEM certificate",
	} {
		_, err := rootCAs(path)
		if err == nil || !strings.Contains(err.Error(), "LDAP_CA_CERT") || !strings.Contains(err.Error(), want) {
			t.Errorf("rootCAs(%s) = %v, want an error naming LDAP_CA_CERT and %q", path, err, want)
		}
	}
}

// testAuthority is a certificate authority of the test's own, as a PEM file
// holds it, and a server certificate it issued for the IP address host.
func testAuthority(t *testing.T, host string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Example Corp Root CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = x509.ParseCertificate(caDER); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(host)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
}

// A directory address is lowercased A to Z only, as every other way in does
// (helpers.NormalizeEmail). Unicode lowercasing folded the Kelvin sign in
// "Kate@corp.example" into an ordinary "k" before sign-in's ASCII check
// saw it, so the entry reached kate@corp.example's account. Left as written,
// the check refuses it.
func TestADirectoryAddressIsNotFoldedOutOfASCII(t *testing.T) {
	kelvin := userFromEntry(ldap.NewEntry("uid=x,dc=corp,dc=example", map[string][]string{
		"uid": {"x"}, "mail": {"Kate@Corp.Example"},
	}), "memberOf")
	if kelvin.Email != "Kate@corp.example" {
		t.Fatalf("got %q: a non-ASCII letter must survive for the ASCII check to refuse it", kelvin.Email)
	}
	plain := userFromEntry(ldap.NewEntry("uid=k,dc=corp,dc=example", map[string][]string{
		"uid": {"k"}, "mail": {"Kate@Corp.Example"},
	}), "memberOf")
	if plain.Email != "kate@corp.example" {
		t.Fatalf("got %q, want kate@corp.example", plain.Email)
	}
}
