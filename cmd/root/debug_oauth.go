package root

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

func newDebugOAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oauth",
		Short: "OAuth token management",
	}

	cmd.AddCommand(newDebugOAuthListCmd())
	cmd.AddCommand(newDebugOAuthRemoveCmd())
	cmd.AddCommand(newDebugOAuthLoginCmd())

	return cmd
}

func newDebugOAuthListCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all stored OAuth tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (commandErr error) {
			ctx := cmd.Context()
			telemetry.TrackCommand(ctx, "debug", []string{"oauth", "list"})
			defer func() {
				telemetry.TrackCommandError(ctx, "debug", []string{"oauth", "list"}, commandErr)
			}()

			w := cmd.OutOrStdout()

			entries, err := mcp.ListOAuthTokens()
			if err != nil {
				return fmt.Errorf("failed to list OAuth tokens: %w", err)
			}

			if len(entries) == 0 {
				if jsonOutput {
					return json.NewEncoder(w).Encode([]any{})
				}
				fmt.Fprintln(w, "No OAuth tokens stored.")
				return nil
			}

			if jsonOutput {
				return printOAuthListJSON(w, entries)
			}

			printOAuthListText(w, entries)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	return cmd
}

type oauthListEntry struct {
	ResourceURL  string    `json:"resource_url"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Expired      bool      `json:"expired"`
	AccessToken  string    `json:"access_token"`
	RefreshToken bool      `json:"has_refresh_token"`
}

func printOAuthListJSON(w io.Writer, entries []mcp.OAuthTokenEntry) error {
	var out []oauthListEntry
	for _, e := range entries {
		out = append(out, oauthListEntry{
			ResourceURL:  e.ResourceURL,
			TokenType:    e.Token.TokenType,
			Scope:        e.Token.Scope,
			ExpiresAt:    e.Token.ExpiresAt,
			Expired:      e.Token.IsExpired(),
			AccessToken:  truncateToken(e.Token.AccessToken),
			RefreshToken: e.Token.RefreshToken != "",
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out) //nolint:gosec // debug command intentionally surfaces (truncated) access tokens
}

func printOAuthListText(w io.Writer, entries []mcp.OAuthTokenEntry) {
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "Resource:       %s\n", e.ResourceURL)
		if e.Token.TokenType != "" {
			fmt.Fprintf(w, "Token Type:     %s\n", e.Token.TokenType)
		}
		if e.Token.Scope != "" {
			fmt.Fprintf(w, "Scope:          %s\n", e.Token.Scope)
		}
		fmt.Fprintf(w, "Access Token:   %s\n", truncateToken(e.Token.AccessToken))
		fmt.Fprintf(w, "Refresh Token:  %v\n", e.Token.RefreshToken != "")
		if !e.Token.ExpiresAt.IsZero() {
			fmt.Fprintf(w, "Expires at:     %s\n", e.Token.ExpiresAt.Local().Format(time.RFC3339))
		}
		if e.Token.IsExpired() {
			fmt.Fprintln(w, "Status:         ❌ Expired")
		} else {
			fmt.Fprintln(w, "Status:         ✅ Valid")
		}
	}
}

func truncateToken(token string) string {
	const previewLen = 10
	if len(token) <= previewLen*2 {
		return token
	}
	return token[:previewLen] + "..." + token[len(token)-previewLen:]
}

func newDebugOAuthRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <resource-url>",
		Short: "Remove a stored OAuth token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (commandErr error) {
			ctx := cmd.Context()
			telemetry.TrackCommand(ctx, "debug", []string{"oauth", "remove"})
			defer func() {
				telemetry.TrackCommandError(ctx, "debug", []string{"oauth", "remove"}, commandErr)
			}()

			if err := mcp.RemoveOAuthToken(args[0]); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Removed OAuth token for %s\n", args[0])
			return nil
		},
	}
}

func newDebugOAuthLoginCmd() *cobra.Command {
	var flags debugFlags

	cmd := &cobra.Command{
		Use:   "login <agent-file> <mcp-name>",
		Short: "Perform OAuth login for a remote MCP server",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (commandErr error) {
			ctx := cmd.Context()
			telemetry.TrackCommand(ctx, "debug", []string{"oauth", "login"})
			defer func() {
				telemetry.TrackCommandError(ctx, "debug", []string{"oauth", "login"}, commandErr)
			}()

			agentFile := args[0]
			mcpName := args[1]

			// Load the agent config to find the MCP server URL.
			agentSource, err := config.Resolve(agentFile, flags.runConfig.EnvProvider())
			if err != nil {
				return err
			}

			cfg, err := config.Load(ctx, agentSource)
			if err != nil {
				return err
			}

			serverURL, err := findMCPRemoteURL(cfg, mcpName)
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Starting OAuth login for %s (%s)...\n", mcpName, serverURL)

			if err := mcp.PerformOAuthLogin(ctx, serverURL); err != nil {
				return fmt.Errorf("OAuth login failed: %w", err)
			}

			fmt.Fprintf(w, "✅ OAuth login successful for %s\n", serverURL)
			return nil
		},
	}

	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

// findMCPRemoteURL looks up the remote URL for the named MCP server in the config.
// It matches by name (top-level mcps key or toolset name), by URL substring,
// or returns the only remote MCP if there is exactly one.
func findMCPRemoteURL(cfg *latest.Config, name string) (string, error) {
	// Collect all remote MCP URLs with their identifiers.
	type mcpEntry struct {
		label string
		url   string
	}
	var all []mcpEntry

	for k, m := range cfg.MCPs {
		if m.Remote.URL != "" {
			all = append(all, mcpEntry{label: k, url: m.Remote.URL})
		}
	}
	for _, agent := range cfg.Agents {
		for _, ts := range agent.Toolsets {
			if ts.Type == "mcp" && ts.Remote.URL != "" {
				label := ts.Name
				if label == "" {
					label = ts.Remote.URL
				}
				all = append(all, mcpEntry{label: label, url: ts.Remote.URL})
			}
		}
	}

	// Exact match by name/label.
	for _, e := range all {
		if e.label == name {
			return e.url, nil
		}
	}

	// Exact match by URL.
	for _, e := range all {
		if e.url == name {
			return e.url, nil
		}
	}

	// Build helpful error.
	var labels []string
	for _, e := range all {
		labels = append(labels, e.label)
	}
	if len(labels) > 0 {
		return "", fmt.Errorf("MCP %q not found; available: %v", name, labels)
	}
	return "", fmt.Errorf("MCP %q not found; no remote MCPs found in config", name)
}
