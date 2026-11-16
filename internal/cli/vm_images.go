package cli

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

const maxBuildContextBytes = 96 << 20

var vmImageCmd = &cobra.Command{
	Use:   "vm-images",
	Short: "Manage VM images saved from VMs or built from Dockerfiles",
}

var vmImageSaveCmd = &cobra.Command{
	Use:   "save <vm> <name>",
	Short: "Save a stopped VM's root disk as a VM image",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		labels, err := labelFlags(cmd)
		if err != nil {
			return err
		}
		return createVMImage(cmd, models.CreateVMImageRequest{Name: args[1], FromVM: args[0], Labels: labels})
	},
}

var vmImageBuildCmd = &cobra.Command{
	Use:   "build <name>",
	Short: "Build a VM image from a Dockerfile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req, err := parseVMImageBuildCommand(cmd, args[0])
		if err != nil {
			return err
		}
		return createVMImage(cmd, req)
	},
}

var vmImageListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List VM images",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		selectors, _ := cmd.Flags().GetStringArray("label")
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var images []models.VMImage
		if err := c.Get(withLabelQuery("/vm-images", selectors), &images); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(images)
		}
		var rows [][]string
		for _, img := range images {
			rows = append(rows, []string{
				img.Name,
				img.Profile,
				string(img.Source),
				fmt.Sprintf("%dMB", img.DiskSizeMB),
				formatLabels(img.Labels),
				img.CreatedAt.Local().Format(time.DateTime),
			})
		}
		printTable([]string{"NAME", "PROFILE", "SOURCE", "DISK", "LABELS", "CREATED"}, rows)
		return nil
	},
}

var vmImageGetCmd = &cobra.Command{
	Use:   "get <name-or-id>",
	Short: "Show a VM image",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		var img models.VMImage
		if err := c.Get("/vm-images/"+url.PathEscape(args[0]), &img); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(img)
	},
}

var vmImageDeleteCmd = &cobra.Command{
	Use:   "delete <name-or-id>",
	Short: "Delete a VM image",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if err := c.Delete("/vm-images/" + url.PathEscape(args[0])); err != nil {
			return err
		}
		fmt.Printf("Deleted VM image %s\n", args[0])
		return nil
	},
}

func createVMImage(cmd *cobra.Command, req models.CreateVMImageRequest) error {
	c, err := NewClientFromConfig()
	if err != nil {
		return err
	}
	extendClientTimeoutForLongOperation(c, image.DefaultBuildTimeout)
	var img models.VMImage
	if err := c.Post("/vm-images", &req, &img); err != nil {
		return err
	}
	if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(img); err != nil {
			return fmt.Errorf("write VM image JSON: %w", err)
		}
		return nil
	}
	fmt.Printf("Created VM image %s (profile: %s, disk: %dMB)\n", img.Name, img.Profile, img.DiskSizeMB)
	return nil
}

func parseVMImageBuildCommand(cmd *cobra.Command, name string) (models.CreateVMImageRequest, error) {
	dockerfilePath, _ := cmd.Flags().GetString("file")
	contextDir, _ := cmd.Flags().GetString("context")
	profile, _ := cmd.Flags().GetString("profile")
	disk, _ := cmd.Flags().GetString("disk")
	labels, err := labelFlags(cmd)
	if err != nil {
		return models.CreateVMImageRequest{}, err
	}
	diskSizeMB, err := parseDiskMB(disk)
	if err != nil {
		return models.CreateVMImageRequest{}, err
	}
	if dockerfilePath == "" {
		if contextDir == "" {
			return models.CreateVMImageRequest{}, fmt.Errorf("--file is required when --context is not set")
		}
		dockerfilePath = filepath.Join(contextDir, "Dockerfile")
	}
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return models.CreateVMImageRequest{}, fmt.Errorf("read Dockerfile: %w", err)
	}
	req := models.CreateVMImageRequest{
		Name:       name,
		Dockerfile: string(dockerfile),
		Profile:    profile,
		DiskSizeMB: diskSizeMB,
		Labels:     labels,
	}
	if contextDir != "" {
		archive, err := archiveBuildContext(contextDir)
		if err != nil {
			return models.CreateVMImageRequest{}, err
		}
		req.Context = archive
	}
	return req, nil
}

func archiveBuildContext(dir string) (string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := addBuildContextEntry(tw, dir, path, entry); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if buf.Len() > maxBuildContextBytes {
			return fmt.Errorf("build context is larger than %d MiB", maxBuildContextBytes>>20)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("archive build context: %w", err)
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("archive build context: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func addBuildContextEntry(tw *tar.Writer, dir, path string, entry fs.DirEntry) error {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return fmt.Errorf("relative path: %w", err)
	}
	if rel == "." {
		return nil
	}
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return fmt.Errorf("read link: %w", err)
		}
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("tar header: %w", err)
	}
	header.Name = filepath.ToSlash(rel)
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

func labelFlags(cmd *cobra.Command) (map[string]string, error) {
	values, _ := cmd.Flags().GetStringArray("label")
	if len(values) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--label %q must be key=value", value)
		}
		labels[key] = val
	}
	return labels, nil
}

func withLabelQuery(path string, selectors []string) string {
	if len(selectors) == 0 {
		return path
	}
	query := url.Values{"label": selectors}
	return path + "?" + query.Encode()
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func init() {
	vmImageCmd.AddCommand(vmImageSaveCmd, vmImageBuildCmd, vmImageListCmd, vmImageGetCmd, vmImageDeleteCmd)
	rootCmd.AddCommand(vmImageCmd)

	for _, cmd := range []*cobra.Command{vmImageSaveCmd, vmImageBuildCmd} {
		cmd.Flags().StringArray("label", nil, "Label as key=value; repeat for more labels")
		cmd.Flags().Bool("json", false, "Print JSON output")
	}
	vmImageBuildCmd.Flags().StringP("file", "f", "", "Path to the Dockerfile; defaults to Dockerfile in --context")
	vmImageBuildCmd.Flags().String("context", "", "Build context directory, sent as a tar archive")
	vmImageBuildCmd.Flags().String("profile", "", "Firecracker Linux profile for VMs from this image (server default debian)")
	vmImageBuildCmd.Flags().String("disk", "", "Root disk size in MiB, or with M/G suffix (profile default when omitted)")
	vmImageListCmd.Flags().StringArray("label", nil, "Show images with this label, as key=value or key; repeat to require all")
	vmImageListCmd.Flags().Bool("json", false, "Print JSON output")
}
