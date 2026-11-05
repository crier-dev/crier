package client

import (
	"net/http"
	"sort"
	"strings"

	"github.com/crier-dev/crier/internal/a2a"
)

// authorize applies the credentials that satisfy one of the card's
// securityRequirements to req.
//
// A nil card means "no card has been read yet" (discovery): only crier's own
// bearerAuth is attachable, which applyDiscoveryCredential handles.
//
// It refuses rather than sending an unauthenticated request when the card
// requires a scheme this client cannot satisfy — that is the CREDENTIAL_REQUIRED
// path, and it must happen before the peer sees a request.
func (c *Client) authorize(req *http.Request, card *a2a.AgentCard) error {
	if card == nil {
		return nil
	}
	creds, satisfied := c.matchedCredentials(card)
	if !satisfied {
		return credentialRequired(card)
	}
	for _, cred := range creds {
		if err := applyScheme(req, card, cred); err != nil {
			return err
		}
	}
	return nil
}

// matchedCredentials returns the credentials that satisfy the card's posture,
// and whether the posture can be satisfied at all.
//
// A2A's `securityRequirements` is a list of ALTERNATIVES, and the schemes
// inside one entry are all required together (§4.5.1). So: with no
// requirements, any bearer credential this client holds is presented (a peer
// with auth on but no requirement in its card still reads the header); with
// requirements, the FIRST fully satisfiable alternative is used.
func (c *Client) matchedCredentials(card *a2a.AgentCard) ([]Credential, bool) {
	if len(card.SecurityRequirements) == 0 {
		var out []Credential
		for _, cred := range c.creds {
			if isBearer(card, cred.Scheme) {
				out = append(out, cred)
			}
		}
		return out, true
	}
	for _, requirement := range card.SecurityRequirements {
		if len(requirement.Schemes) == 0 {
			continue
		}
		var (
			out []Credential
			ok  = true
		)
		for name := range requirement.Schemes {
			// A requirement naming a scheme the card itself does not declare
			// cannot be honoured by anyone: it is not satisfiable, whatever
			// credentials this client holds.
			if _, declared := card.SecuritySchemes[name]; !declared {
				ok = false
				break
			}
			cred, found := c.find(name)
			if !found {
				ok = false
				break
			}
			out = append(out, cred)
		}
		if ok {
			return out, true
		}
	}
	return nil, false
}

// find returns the credential for a scheme name.
func (c *Client) find(scheme string) (Credential, bool) {
	for _, cred := range c.creds {
		if cred.Scheme == scheme {
			return cred, true
		}
	}
	return Credential{}, false
}

// applyScheme writes one credential onto req in the shape the card's own
// securitySchemes entry describes: an HTTP bearer goes in Authorization, an
// apiKey-style scheme goes in the header the card names. A scheme this client
// cannot express is a named refusal, never a silently unauthenticated request.
func applyScheme(req *http.Request, card *a2a.AgentCard, cred Credential) error {
	if def, declared := card.SecuritySchemes[cred.Scheme]; declared {
		switch {
		case def.HTTPAuth != nil && strings.EqualFold(strings.TrimSpace(def.HTTPAuth.Scheme), "bearer"):
			req.Header.Set("Authorization", "Bearer "+cred.secret)
			return nil
		case def.APIKey != nil && strings.TrimSpace(def.APIKey.Name) != "":
			req.Header.Set(strings.TrimSpace(def.APIKey.Name), cred.secret)
			return nil
		}
		return newError("send", NameCredentialRequired, "the card declares scheme "+cred.Scheme+
			" in a shape this client cannot present (only an HTTP bearer or an apiKey header)", nil)
	}
	// Undeclared but named as crier's own bearer: the same header crier's
	// OpenAPI components describe.
	if cred.Scheme == bearerAuthSchemeName {
		req.Header.Set("Authorization", "Bearer "+cred.secret)
		return nil
	}
	return newError("send", NameCredentialRequired, "the card does not declare scheme "+cred.Scheme, nil)
}

// isBearer reports whether scheme is an HTTP bearer according to the card (or
// crier's own name for one).
func isBearer(card *a2a.AgentCard, scheme string) bool {
	if def, ok := card.SecuritySchemes[scheme]; ok && def.HTTPAuth != nil {
		return strings.EqualFold(strings.TrimSpace(def.HTTPAuth.Scheme), "bearer")
	}
	return scheme == bearerAuthSchemeName
}

// sortedStrings returns a sorted copy, so an error naming a set is stable
// between runs (a map iteration is not).
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
