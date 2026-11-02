package scom

import (
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// BadRequestError marks a problem with the query itself (an id that isn't
// one, an unknown option) rather than with the databases or the plugin. The
// plugin reports these as HTTP 400.
type BadRequestError struct{ msg string }

func (e *BadRequestError) Error() string { return e.msg }

func badRequestf(format string, args ...any) error {
	return &BadRequestError{msg: fmt.Sprintf(format, args...)}
}

// BadRequestf returns a BadRequestError, for callers outside this package.
func BadRequestf(format string, args ...any) error {
	return badRequestf(format, args...)
}

// IsBadRequest reports whether err, or anything it wraps, is a
// BadRequestError.
func IsBadRequest(err error) bool {
	var bad *BadRequestError
	return errors.As(err, &bad)
}

var guidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// ValidateIDs checks that every non-empty id is a GUID, the form SCOM class,
// group and instance ids take everywhere in this plugin. Anything else would
// only fail later as a SQL conversion error, and the usual cause is a
// dashboard variable that didn't resolve, so the message says so.
func ValidateIDs(kind string, ids ...string) error {
	for _, id := range ids {
		if id == "" || guidPattern.MatchString(id) {
			continue
		}
		if strings.Contains(id, "$") {
			return badRequestf("%s %q isn't a SCOM id. Check that the dashboard variable exists and lists SCOM ids", kind, id)
		}
		return badRequestf("%s %q isn't a SCOM id", kind, id)
	}
	return nil
}

// parseCounterIDs parses the query's counters, which are Data Warehouse
// PerformanceRuleInstanceRowIds (integers).
func parseCounterIDs(ids []string) ([]int64, error) {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil {
			if strings.Contains(id, "$") {
				return nil, badRequestf("counter %q isn't a SCOM counter id. Check that the dashboard variable exists", id)
			}
			return nil, badRequestf("counter %q isn't a SCOM counter id", id)
		}
		out = append(out, n)
	}
	return out, nil
}

// IsDownstreamError reports whether err came from SQL Server or the network
// (a SQL error, a failed login, an unreachable host, a timeout) rather than
// from the plugin. Grafana uses the distinction to tell plugin bugs apart
// from problems with the data source it talks to.
func IsDownstreamError(err error) bool {
	if err == nil {
		return false
	}
	var sqlErr mssql.Error
	var serverErr mssql.ServerError
	var streamErr mssql.StreamError
	var netErr net.Error
	switch {
	case errors.As(err, &sqlErr), errors.As(err, &serverErr), errors.As(err, &streamErr), errors.As(err, &netErr):
		return true
	case isContextError(err), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	// Some driver errors (failed logins, TLS failures) only keep the message.
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"login error", "unable to open tcp connection", "tls handshake failed", "connection reset", "connection refused"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ConnectionHint suggests a fix for a connection or probe failure, or
// returns "" if the error isn't one it recognises. warehouse says which
// database the error came from, since the right database name differs.
func ConnectionHint(err error, warehouse bool) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "invalid object name"):
		if warehouse {
			return "This doesn't look like the SCOM Data Warehouse. Check the database name (usually OperationsManagerDW)."
		}
		return "This doesn't look like the SCOM Operational database. Check the database name (usually OperationsManager)."
	case strings.Contains(msg, "cannot open database"):
		return "The login can't open that database. Check the database name and that the login has access to it."
	case strings.Contains(msg, "permission was denied"):
		return "The login connects but can't read SCOM's tables. Give it read access, for example the db_datareader role."
	case strings.Contains(msg, "krb5"), strings.Contains(msg, "kerberos"):
		return "Check the Kerberos realm, the Kerberos configuration on the Grafana server and the SQL Server's service principal name (SPN)."
	case strings.Contains(msg, "login failed"):
		return "Check the username, password and authentication type (and the domain for Windows/NTLM)."
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"), strings.Contains(msg, "tls handshake"):
		return "The server's TLS certificate isn't trusted. Install a trusted certificate, or turn on Trust server certificate on a network you trust."
	case strings.Contains(msg, "no such host"):
		return "The host name can't be resolved. Check the Host field."
	case strings.Contains(msg, "unable to open tcp connection"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "connectex"):
		return "The server can't be reached. Check the host, port and instance name, and that firewalls allow the connection (TCP 1433 by default; UDP 1434 for a named instance without a port)."
	}
	return ""
}
