package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

var vmUploadCmd = &cobra.Command{
	Use:   "upload <name-or-id> <local-path> <guest-path>",
	Short: "Copy a local file into a persistent VM, waking it when policy allows",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		mode, _ := cmd.Flags().GetString("mode")
		result, err := c.Upload(args[0], args[1], args[2], mode)
		if err != nil {
			return err
		}
		fmt.Printf("Uploaded %d bytes to %s:%s\n", result.Bytes, args[0], result.Path)
		return nil
	},
}

type uploadResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Mode  string `json:"mode"`
}

func (c *Client) Upload(ref, localPath, guestPath, mode string) (*uploadResult, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("open upload source: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", localPath)
	}
	if mode == "" {
		mode = strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	}
	query := url.Values{"path": {guestPath}, "mode": {mode}}
	req, err := c.newRequest(http.MethodPut, "/vms/"+url.PathEscape(ref)+"/files?"+query.Encode(), file)
	if err != nil {
		return nil, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	var result uploadResult
	if err := c.send(req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func init() {
	vmUploadCmd.Flags().String("mode", "", "Octal permissions for the guest file; defaults to the local file's permissions")
	rootCmd.AddCommand(vmUploadCmd)
}
