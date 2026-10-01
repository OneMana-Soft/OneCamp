package ldap

import (
	"crypto/tls"
	"fmt"
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
		conn, err = ldap.DialTLS("tcp", address, &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         lc.Host,
		})
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

	// Extract standard attributes securely
	email := userEntry.GetAttributeValue("mail")
	if email == "" {
		email = userEntry.GetAttributeValue("userPrincipalName")
	}
	if email == "" {
		// Fallback if LDAP has username but no email attribute: compile with host domain
		email = fmt.Sprintf("%s@ldap.local", usernameOrEmail)
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
		DN:       userDN,
		Email:    strings.ToLower(email),
		Username: username,
		FullName: fullName,
		Groups:   userEntry.GetAttributeValues(groupAttr),
	}, nil
}
