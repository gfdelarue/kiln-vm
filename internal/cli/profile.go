package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

var profilePrepareCmd = &cobra.Command{
	Use:   "prepare <profile>",
	Short: "Prepare a VM profile from user-provided media",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		strategy, _ := cmd.Flags().GetString("strategy")
		media, _ := cmd.Flags().GetString("media")
		virtioMedia, _ := cmd.Flags().GetString("virtio-media")
		productKey, _ := cmd.Flags().GetString("product-key")
		productKeyFile, _ := cmd.Flags().GetString("product-key-file")
		activationMode, _ := cmd.Flags().GetString("activation-mode")
		timeoutSeconds, _ := cmd.Flags().GetInt("timeout-seconds")
		refresh, _ := cmd.Flags().GetBool("refresh")
		jsonOutput, _ := cmd.Flags().GetBool("json")
		extendClientTimeoutForLongOperation(c, time.Duration(timeoutSeconds)*time.Second)
		req := models.PrepareVMProfileRequest{
			Strategy:           strategy,
			MediaPath:          media,
			VirtIOMediaPath:    virtioMedia,
			ProductKey:         productKey,
			ProductKeyFile:     productKeyFile,
			ActivationMode:     activationMode,
			Refresh:            refresh,
			WaitTimeoutSeconds: timeoutSeconds,
		}
		var image models.VMProfileImage
		if err := c.Post("/vm-profiles/"+args[0]+"/prepare", &req, &image); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(image)
		}
		fmt.Printf("Prepared %s profile image %s (image: %s, size: %dMB)\n", image.Profile, image.ID, image.ImageID, image.RootfsSizeMB)
		return nil
	},
}

func init() {
	vmProfilesCmd.AddCommand(profilePrepareCmd)
	profilePrepareCmd.Flags().String("strategy", "", "Preparation strategy override; defaults to the profile strategy")
	profilePrepareCmd.Flags().String("media", "", "Path to installer media for media-based preparation strategies")
	profilePrepareCmd.Flags().String("virtio-media", "", "Path to a virtio-win ISO; defaults to a pinned release that kiln-vmd downloads and verifies")
	profilePrepareCmd.Flags().String("product-key", "", "Product key for strategies that require licensed media")
	profilePrepareCmd.Flags().String("product-key-file", "", "Path to file containing a product key")
	profilePrepareCmd.Flags().String("activation-mode", "", "Activation mode for strategies that support licensing")
	profilePrepareCmd.Flags().Int("timeout-seconds", 5400, "Profile preparation timeout in seconds")
	profilePrepareCmd.Flags().Bool("refresh", false, "Rebuild the prepared profile image")
	profilePrepareCmd.Flags().Bool("json", false, "Print JSON output")
}

func extendClientTimeoutForLongOperation(c *Client, operationTimeout time.Duration) {
	if c == nil || c.HTTPClient == nil || operationTimeout <= 0 {
		return
	}
	required := operationTimeout + 2*time.Minute
	if c.HTTPClient.Timeout < required {
		c.HTTPClient.Timeout = required
	}
}
