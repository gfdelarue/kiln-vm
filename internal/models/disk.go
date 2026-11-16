package models

type DiskUsageEntry struct {
	Kind             string `json:"kind"`
	ID               string `json:"id,omitempty"`
	Name             string `json:"name,omitempty"`
	Path             string `json:"path"`
	State            string `json:"state,omitempty"`
	Referenced       bool   `json:"referenced"`
	Reclaimable      bool   `json:"reclaimable"`
	Reason           string `json:"reason,omitempty"`
	ApparentBytes    uint64 `json:"apparent_bytes"`
	AllocatedBytes   uint64 `json:"allocated_bytes"`
	ReclaimableBytes uint64 `json:"reclaimable_bytes"`
}

type DiskUsageTotals struct {
	ApparentBytes    uint64 `json:"apparent_bytes"`
	AllocatedBytes   uint64 `json:"allocated_bytes"`
	ReclaimableBytes uint64 `json:"reclaimable_bytes"`
}

type DiskUsageReport struct {
	DataDir  string             `json:"data_dir"`
	BuildDir string             `json:"build_dir,omitempty"`
	Entries  []DiskUsageEntry   `json:"entries"`
	Totals   DiskUsageTotals    `json:"totals"`
	Warnings []DiskUsageWarning `json:"warnings,omitempty"`
}

type DiskUsageWarning struct {
	Code             string `json:"code"`
	Severity         string `json:"severity"`
	Message          string `json:"message"`
	Path             string `json:"path,omitempty"`
	AvailableBytes   uint64 `json:"available_bytes,omitempty"`
	TotalBytes       uint64 `json:"total_bytes,omitempty"`
	AllocatedBytes   uint64 `json:"allocated_bytes,omitempty"`
	ReclaimableBytes uint64 `json:"reclaimable_bytes,omitempty"`
}

type CleanupRequest struct {
	DryRun bool `json:"dry_run"`
}

type CleanupReport struct {
	DryRun  bool             `json:"dry_run"`
	Usage   DiskUsageReport  `json:"usage"`
	Removed []DiskUsageEntry `json:"removed"`
	Failed  []DiskUsageEntry `json:"failed"`
}
