package scom

import (
	"fmt"
	"net/url"

	// Blank-imported so their init() registers the "ntlm" and "krb5"
	// integratedauth providers with go-mssqldb. Without these, AuthType
	// ntlm/kerberos connection strings would fail with "provider not found".
	_ "github.com/microsoft/go-mssqldb/integratedauth/krb5"
	_ "github.com/microsoft/go-mssqldb/integratedauth/ntlm"

	"github.com/a-logan/scom-datasource/pkg/models"
)

// BuildDSN builds a sqlserver:// connection string for the mssql driver from
// a ConnectionSettings and its decrypted password.
func BuildDSN(cs models.ConnectionSettings, password string) (string, error) {
	if cs.Host == "" {
		return "", fmt.Errorf("host is required")
	}
	if cs.Database == "" {
		return "", fmt.Errorf("database is required")
	}

	host := cs.Host
	if cs.Port != "" {
		host = fmt.Sprintf("%s:%s", cs.Host, cs.Port)
	}

	// A named SQL Server instance (e.g. "SQLSERVER\SCOM") is passed as a URL
	// path segment rather than part of Host. If Port is also set, the driver
	// connects to it directly; otherwise it falls back to resolving the
	// instance's dynamic port via the SQL Browser service (UDP 1434), which
	// production firewalls often block, so setting Port is recommended
	// whenever InstanceName is used.
	var path string
	if cs.InstanceName != "" {
		path = "/" + cs.InstanceName
	}

	q := url.Values{}
	q.Set("database", cs.Database)
	if cs.Encrypt != "" {
		q.Set("encrypt", cs.Encrypt)
	}
	if cs.TrustServerCertificate {
		q.Set("trustservercertificate", "true")
	}

	username := cs.Username

	switch cs.AuthType {
	case models.AuthTypeSQL, "":
		// username/password as-is.
	case models.AuthTypeNTLM:
		if cs.Domain == "" {
			return "", fmt.Errorf("domain is required for NTLM authentication")
		}
		q.Set("authenticator", "ntlm")
		username = fmt.Sprintf(`%s\%s`, cs.Domain, cs.Username)
	case models.AuthTypeKerberos:
		if cs.Realm == "" {
			return "", fmt.Errorf("realm is required for Kerberos authentication")
		}
		q.Set("authenticator", "krb5")
		q.Set("krb5-realm", cs.Realm)
	default:
		return "", fmt.Errorf("unsupported auth type: %s", cs.AuthType)
	}

	u := url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(username, password),
		Host:     host,
		Path:     path,
		RawQuery: q.Encode(),
	}

	return u.String(), nil
}
