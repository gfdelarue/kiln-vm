package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var loginCmd = newLoginCommand()

func newLoginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store API URL and token",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiURL, err := cmd.Flags().GetString("api-url")
			if err != nil {
				return fmt.Errorf("read --api-url: %w", err)
			}
			token, err := cmd.Flags().GetString("token")
			if err != nil {
				return fmt.Errorf("read --token: %w", err)
			}
			allowInsecureHTTP, err := cmd.Flags().GetBool("allow-insecure-http")
			if err != nil {
				return fmt.Errorf("read --allow-insecure-http: %w", err)
			}
			allowFlagSet := cmd.Flags().Changed("allow-insecure-http")
			apiURLFlagSet := cmd.Flags().Changed("api-url")
			tokenFlagSet := cmd.Flags().Changed("token")

			// Load the saved file first, then apply process environment values.
			// API URL and token environment values participate in login's merged
			// input, while insecure-HTTP consent remains durable only when it was
			// already saved for the same URL or this invocation supplies the flag.
			saved, err := loadSavedConfig()
			if err != nil {
				return err
			}
			runtime, err := LoadConfig()
			if err != nil {
				return err
			}
			cfg := &CLIConfig{
				APIURL: runtime.APIURL,
				Token:  runtime.Token,
			}
			if apiURLFlagSet {
				cfg.APIURL = apiURL
			}
			if tokenFlagSet {
				cfg.Token = token
			}
			// Consent is tied to the final URL after file -> environment ->
			// flag precedence, not to an intermediate environment URL.
			cfg.AllowInsecureHTTP = cfg.APIURL == saved.APIURL && saved.AllowInsecureHTTP
			// An explicit environment revocation participates in the runtime
			// merge even when the saved URL did not change. Environment consent
			// can never grant durable consent, but false must still revoke it.
			if value, ok := os.LookupEnv("KILN_VM_ALLOW_INSECURE_HTTP"); ok && value == "false" {
				cfg.AllowInsecureHTTP = false
			}
			if allowFlagSet {
				cfg.AllowInsecureHTTP = allowInsecureHTTP
			}

			if cfg.APIURL == "" || cfg.Token == "" {
				return fmt.Errorf("both --api-url and --token are required")
			}
			if err := validateAPIURL(cfg.APIURL, cfg.AllowInsecureHTTP); err != nil {
				return err
			}

			if err := SaveConfig(cfg); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Saved configuration to %s\n", configPath())
			return nil
		},
	}
	cmd.Flags().String("api-url", "", "API URL (HTTPS or loopback HTTP)")
	cmd.Flags().String("token", "", "API token")
	cmd.Flags().Bool("allow-insecure-http", false, "allow bearer-token authentication over remote plaintext HTTP")
	return cmd
}
