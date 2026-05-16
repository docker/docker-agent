package root

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/desktop"
	"github.com/docker/docker-agent/pkg/telemetry"
)

// authInfo holds the parsed JWT authentication information.
type authInfo struct {
	Token     string    `json:"token"`
	Subject   string    `json:"subject,omitempty"`
	Issuer    string    `json:"issuer,omitempty"`
	IssuedAt  time.Time `json:"issued_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Expired   bool      `json:"expired"`
	Username  string    `json:"username,omitempty"`
	Email     string    `json:"email,omitempty"`
}

func newDebugAuthCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Print Docker Desktop authentication information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (commandErr error) {
			ctx := cmd.Context()
			telemetry.TrackCommand(ctx, "debug", []string{"auth"})
			defer func() { // do not inline this defer so that commandErr is not resolved early
				telemetry.TrackCommandError(ctx, "debug", []string{"auth"}, commandErr)
			}()

			w := cmd.OutOrStdout()

			token := desktop.GetToken(ctx)
			if token == "" {
				if jsonOutput {
					return json.NewEncoder(w).Encode(map[string]string{
						"error": "no token found (is Docker Desktop running and are you logged in?)",
					})
				}
				fmt.Fprintln(w, "No token found. Is Docker Desktop running and are you logged in?")
				return nil
			}

			info, err := parseAuthInfo(token)
			if err != nil {
				return fmt.Errorf("failed to parse JWT: %w", err)
			}

			userInfo := desktop.GetUserInfo(ctx)
			info.Username = userInfo.Username
			info.Email = userInfo.Email

			if jsonOutput {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}

			printAuthInfoText(w, info)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	return cmd
}

func parseAuthInfo(token string) (*authInfo, error) {
	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, err
	}

	info := &authInfo{
		Token: token,
	}

	if sub, err := parsed.Claims.GetSubject(); err == nil {
		info.Subject = sub
	}
	if iss, err := parsed.Claims.GetIssuer(); err == nil {
		info.Issuer = iss
	}
	if iat, err := parsed.Claims.GetIssuedAt(); err == nil && iat != nil {
		info.IssuedAt = iat.Time
	}
	if exp, err := parsed.Claims.GetExpirationTime(); err == nil && exp != nil {
		info.ExpiresAt = exp.Time
		info.Expired = exp.Before(time.Now())
	}

	return info, nil
}

func printAuthInfoText(w io.Writer, info *authInfo) {
	const previewLen = 10
	if len(info.Token) <= previewLen*2 {
		fmt.Fprintf(w, "Token:      %s\n", info.Token)
	} else {
		fmt.Fprintf(w, "Token:      %s...%s\n", info.Token[:previewLen], info.Token[len(info.Token)-previewLen:])
	}

	if info.Username != "" {
		fmt.Fprintf(w, "Username:   %s\n", info.Username)
	}
	if info.Email != "" {
		fmt.Fprintf(w, "Email:      %s\n", info.Email)
	}
	if info.Subject != "" {
		fmt.Fprintf(w, "Subject:    %s\n", info.Subject)
	}
	if info.Issuer != "" {
		fmt.Fprintf(w, "Issuer:     %s\n", info.Issuer)
	}
	if !info.IssuedAt.IsZero() {
		fmt.Fprintf(w, "Issued at:  %s\n", info.IssuedAt.Local().Format(time.RFC3339))
	}
	if !info.ExpiresAt.IsZero() {
		fmt.Fprintf(w, "Expires at: %s\n", info.ExpiresAt.Local().Format(time.RFC3339))
	}

	if info.Expired {
		fmt.Fprintln(w, "Status:     ❌ Expired")
	} else {
		fmt.Fprintln(w, "Status:     ✅ Valid")
	}
}
