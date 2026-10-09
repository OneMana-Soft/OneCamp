package ldap

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/akashc777/OneCamp/helpers"
	"os"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

type LDAPClient struct {
	Host         string
	Port         int
	UseTLS       bool
	BindDN       string
	BindPassword string
	BaseDN       string
	UserFilter   string // expects exactly one %s for the escaped username/email

	// GroupAttribute is the multi-valued attribute on the user entry that
	// lists their group DNs (or names for non-AD directories). Defaults to
	// "memberOf" when empty. Used by the admin-sync flow.
	GroupAttribute string

	// CACertPath is a PEM bundle of certificate authorities LDAPS trusts as
	// well as the system's (LDAP_CA_CERT): a company's own, which an internal
	// directory's certificate usually comes from. Empty trusts the system's.
	CACertPath string
}

type LDAPUser struct {
	DN       string
	Email    string
	Username string
	FullName string
	// Groups are the values of the GroupAttribute on the user entry. For AD
	// these are full DNs (e.g. "CN=OneCamp Admins,OU=Groups,DC=example,DC=com");
	// the admin-sync logic does case-insensitive comparison against the
	// configured allowlist so operators can use either short names or DNs.
	Groups []string
}

func (lc *LDAPClient) Authenticate(usernameOrEmail, password string) (*LDAPUser, error) {
	address := fmt.Sprintf("%s:%d", lc.Host, lc.Port)

	var conn *ldap.Conn
	var err error

	if lc.UseTLS {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         lc.Host,
		}
		if lc.CACertPath != "" {
			if tlsConfig.RootCAs, err = rootCAs(lc.CACertPath); err != nil {
				return nil, err
			}
		}
		conn, err = ldap.DialTLS("tcp", address, tlsConfig)
	} else {
		conn, err = ldap.Dial("tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("ldap connection failed: %w", err)
	}
	defer conn.Close()

	// Bind with read-only system account DN and password to execute searches
	if lc.BindDN != "" {
		if err := conn.Bind(lc.BindDN, lc.BindPassword); err != nil {
			return nil, fmt.Errorf("ldap bind failed: %w", err)
		}
	}

	// Escape parameter to protect against LDAP Injection attacks
	escapedInput := ldap.EscapeFilter(usernameOrEmail)
	filter := fmt.Sprintf(lc.UserFilter, escapedInput)

	groupAttr := lc.GroupAttribute
	if groupAttr == "" {
		groupAttr = "memberOf"
	}

	searchRequest := ldap.NewSearchRequest(
		lc.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter,
		[]string{"dn", "mail", "sAMAccountName", "uid", "cn", "displayName", "userPrincipalName", groupAttr},
		nil,
	)

	sr, err := conn.Search(searchRequest)
	if err != nil {
		return nil, fmt.Errorf("ldap search query failed: %w", err)
	}

	if len(sr.Entries) == 0 {
		return nil, fmt.Errorf("user search yielded no directory entries")
	}
	if len(sr.Entries) > 1 {
		return nil, fmt.Errorf("user search returned multiple directory records")
	}

	userEntry := sr.Entries[0]
	userDN := userEntry.DN

	// Authenticate the user by binding using their retrieved DN and inputted password
	if err := conn.Bind(userDN, password); err != nil {
		return nil, fmt.Errorf("ldap credential bind failed: %w", err)
	}

	return userFromEntry(userEntry, groupAttr), nil
}

// rootCAs is who LDAPS trusts when LDAP_CA_CERT is set: the system's
// certificate authorities and those in the PEM bundle at caCertPath. Read at
// each sign-in, so a replaced bundle is used without a restart. A file that
// can't be read, or holds no certificate, is an error naming LDAP_CA_CERT,
// not a quiet fallback to the system's alone that fails every sign-in with a
// certificate error.
func rootCAs(caCertPath string) (*x509.CertPool, error) {
	bundle, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("LDAP_CA_CERT: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("LDAP_CA_CERT: %s holds no PEM certificate", caCertPath)
	}
	return pool, nil
}

// userFromEntry is the person a directory entry describes. Their address is
// one the directory holds, mail else userPrincipalName, or none: LDAPLogin
// refuses an entry without one. It used to be made up from what was typed,
// "<typed>@ldap.local", an address no mail reaches, which two directories with
// the same user name would share, and which became the account. Pure.
func userFromEntry(userEntry *ldap.Entry, groupAttr string) *LDAPUser {
	email := userEntry.GetAttributeValue("mail")
	if email == "" {
		email = userEntry.GetAttributeValue("userPrincipalName")
	}

	username := userEntry.GetAttributeValue("sAMAccountName")
	if username == "" {
		username = userEntry.GetAttributeValue("uid")
	}
	if username == "" {
		username = strings.Split(email, "@")[0]
	}

	fullName := userEntry.GetAttributeValue("displayName")
	if fullName == "" {
		fullName = userEntry.GetAttributeValue("cn")
	}
	if fullName == "" {
		fullName = username
	}

	return &LDAPUser{
		DN:       userEntry.DN,
		Email:    helpers.NormalizeEmail(email),
		Username: username,
		FullName: fullName,
		Groups:   userEntry.GetAttributeValues(groupAttr),
	}
}
