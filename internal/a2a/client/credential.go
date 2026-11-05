package client

import (
	"fmt"
	"os"
	"strings"
)

// Credential is an outbound A2A credential: the peer's declared security
// scheme name plus the secret that satisfies it.
//
// The secret has NO accessor and NO JSON form. It is read from a FILE or an
// ENVIRONMENT VARIABLE (the two constructors) and is never a plain argument to
// this package's API, so it cannot arrive through a value a caller logs by
// accident. Both String and GoString are redacted, so `%v`, `%+v` and `%s` of
// a Credential — including one embedded in an Options value or an error
// message built with %v — print where it came from, never what it is.
//
// The scheme name is the name the PEER's card uses (§4.5.1's
// `securitySchemes` keys, and the names its `securityRequirements` list). For
// crier's own cards that is "bearerAuth" (a2a.SchemeBearerAuth); another
// server may call its bearer scheme anything, in which case pass that name.
type Credential struct {
	// Scheme is the peer card's security-scheme name.
	Scheme string
	// Source records where the secret came from — "env:NAME" or "file:/path".
	// It is safe to print and is what String reports.
	Source string
	// secret is the resolved value. Never printed, never marshalled.
	secret string
}

// maxCredentialBytes bounds what one credential file may carry. A bearer token
// or an API key is short; anything larger is a mistaken path (a PEM bundle, a
// config file) and is refused rather than silently truncated.
const maxCredentialBytes = 64 << 10

// CredentialFromEnv reads the secret from the named environment variable.
// getenv is injected so a caller (and a test) can supply its own lookup rather
// than reaching into the process environment; a nil getenv means os.Getenv.
//
// An unset or blank variable is a named error: a send with no credential the
// peer requires would be a 401 the caller learns about one round trip later,
// with no clue that the variable was simply missing.
func CredentialFromEnv(scheme, name string, getenv func(string) string) (Credential, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	scheme = strings.TrimSpace(scheme)
	if scheme == "" {
		return Credential{}, newError("credential", NameCredentialInvalid, "a credential needs the peer's security-scheme name", nil)
	}
	if strings.TrimSpace(name) == "" {
		return Credential{}, newError("credential", NameCredentialInvalid, "the environment variable name is empty", nil)
	}
	raw := getenv(name)
	secret := strings.TrimSpace(raw)
	if secret == "" {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf(
			"environment variable %s is unset or empty", name), nil)
	}
	if len(secret) > maxCredentialBytes {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf(
			"the value of %s is %d bytes, over the %d-byte ceiling for a credential", name, len(secret), maxCredentialBytes), nil)
	}
	return Credential{Scheme: scheme, Source: "env:" + name, secret: secret}, nil
}

// CredentialFromFile reads the secret from path. Surrounding whitespace and a
// trailing newline are trimmed, so `echo token > file` works.
//
// A file readable by group or others is REFUSED rather than used: a credential
// that any local user can read is not a credential this client should carry,
// and the refusal names the chmod that fixes it. The check is skipped on
// platforms where the mode carries no permission bits (the file is then read
// as given).
func CredentialFromFile(scheme, path string) (Credential, error) {
	scheme = strings.TrimSpace(scheme)
	if scheme == "" {
		return Credential{}, newError("credential", NameCredentialInvalid, "a credential needs the peer's security-scheme name", nil)
	}
	if strings.TrimSpace(path) == "" {
		return Credential{}, newError("credential", NameCredentialInvalid, "the credential file path is empty", nil)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf("cannot stat credential file %s: %v", path, err), err)
	}
	if !info.Mode().IsRegular() {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf(
			"credential path %s is not a regular file", path), nil)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf(
			"credential file %s is readable by other users (mode %04o) — chmod 600 %s", path, perm, path), nil)
	}
	if info.Size() > maxCredentialBytes {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf(
			"credential file %s is %d bytes, over the %d-byte ceiling for a credential", path, info.Size(), maxCredentialBytes), nil)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf("cannot read credential file %s: %v", path, err), err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return Credential{}, newError("credential", NameCredentialInvalid, fmt.Sprintf("credential file %s is empty", path), nil)
	}
	return Credential{Scheme: scheme, Source: "file:" + path, secret: secret}, nil
}

// String reports the credential's scheme and origin, never its value.
func (c Credential) String() string {
	return fmt.Sprintf("credential{scheme:%s from:%s value:<redacted>}", c.Scheme, c.Source)
}

// GoString keeps `%#v` on the same redacted footing as String.
func (c Credential) GoString() string { return c.String() }

// MarshalJSON makes a Credential that is accidentally serialized (in a record,
// a log line built with %#v→json, a debug payload) carry its provenance only.
func (c Credential) MarshalJSON() ([]byte, error) {
	type public struct {
		Scheme string `json:"scheme"`
		Source string `json:"source"`
		Value  string `json:"value"`
	}
	return marshalJSON(public{Scheme: c.Scheme, Source: c.Source, Value: "<redacted>"})
}

// bearerAuthSchemeName is crier's own name for its HTTP bearer scheme — the
// name its card publishes (§5.3.1) and the name its OpenAPI components use. A
// credential for a crier peer carries exactly this name, which is why it is
// also the one scheme this client will attach to DISCOVERY, before the card
// that would define any other name has been read.
const bearerAuthSchemeName = "bearerAuth"
