package hubauth

// Identity is the Docker account a token was issued for. Both Docker Desktop's
// tokens and the ones we mint carry it, which makes the account known without
// asking Docker Desktop — the only source docker-agent used to have.
type Identity struct {
	Username string
	Email    string
}

// hubClaim is the namespaced claim Docker's tokens carry their account
// information in.
const hubClaim = "https://hub.docker.com"

// IdentityFromToken returns the account token was issued for, and false when
// the token carries no account information.
func IdentityFromToken(token string) (Identity, bool) {
	claims, err := parseClaims(token)
	if err != nil {
		return Identity{}, false
	}
	fields, ok := claims[hubClaim].(map[string]any)
	if !ok {
		return Identity{}, false
	}

	identity := Identity{
		Username: stringField(fields, "username"),
		Email:    stringField(fields, "email"),
	}
	if identity.Username == "" && identity.Email == "" {
		return Identity{}, false
	}
	return identity, true
}

func stringField(fields map[string]any, name string) string {
	value, _ := fields[name].(string)
	return value
}
