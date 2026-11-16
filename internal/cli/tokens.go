package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

var tokensCmd = &cobra.Command{
	Use:   "tokens",
	Short: "Manage API tokens",
}

var tokensListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all tokens",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}

		var tokens []models.Token
		if err := c.Get("/tokens", &tokens); err != nil {
			return err
		}

		headers := []string{"ID", "NAME", "CREATED", "EXPIRES"}
		var rows [][]string
		for _, t := range tokens {
			expires := "-"
			if t.ExpiresAt != nil {
				expires = t.ExpiresAt.Format("2006-01-02 15:04:05")
			}
			rows = append(rows, []string{
				t.ID, t.Name,
				t.CreatedAt.Format("2006-01-02 15:04:05"),
				expires,
			})
		}
		printTable(headers, rows)
		return nil
	},
}

var tokensCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new token",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}

		name, _ := cmd.Flags().GetString("name")
		expiresIn, _ := cmd.Flags().GetInt("expires-in")

		req := models.CreateTokenRequest{Name: name}
		if expiresIn > 0 {
			req.ExpiresIn = &expiresIn
		}

		var resp models.CreateTokenResponse
		if err := c.Post("/tokens", &req, &resp); err != nil {
			return err
		}

		fmt.Printf("Token: %s\n", resp.PlainToken)
		fmt.Printf("ID:    %s\n", resp.ID)
		fmt.Printf("Name:  %s\n", resp.Name)
		return nil
	},
}

var tokensDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a token",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}

		if err := c.Delete("/tokens/" + args[0]); err != nil {
			return err
		}

		fmt.Printf("Deleted token %s\n", args[0])
		return nil
	},
}

func init() {
	tokensCmd.AddCommand(tokensListCmd)
	tokensCmd.AddCommand(tokensCreateCmd)
	tokensCmd.AddCommand(tokensDeleteCmd)

	tokensCreateCmd.Flags().String("name", "", "Token name")
	tokensCreateCmd.Flags().Int("expires-in", 0, "Expiration in seconds (0 = never)")
	tokensCreateCmd.MarkFlagRequired("name")
}
