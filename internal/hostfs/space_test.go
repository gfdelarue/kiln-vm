package hostfs

import "testing"

func TestUsageAtLeastPercent(t *testing.T) {
	maxUint64 := ^uint64(0)
	tests := []struct {
		name       string
		snapshot   FilesystemSnapshot
		percentage uint64
		want       bool
	}{
		{
			name:       "below threshold",
			snapshot:   FilesystemSnapshot{TotalBytes: 1000, AvailableBytes: 151},
			percentage: 85,
			want:       false,
		},
		{
			name:       "exact threshold",
			snapshot:   FilesystemSnapshot{TotalBytes: 1000, AvailableBytes: 150},
			percentage: 85,
			want:       true,
		},
		{
			name:       "above threshold",
			snapshot:   FilesystemSnapshot{TotalBytes: 1000, AvailableBytes: 149},
			percentage: 85,
			want:       true,
		},
		{
			name:       "zero threshold disabled",
			snapshot:   FilesystemSnapshot{TotalBytes: 1000, AvailableBytes: 150},
			percentage: 0,
			want:       false,
		},
		{
			name:       "zero total disabled",
			snapshot:   FilesystemSnapshot{TotalBytes: 0, AvailableBytes: 0},
			percentage: 85,
			want:       false,
		},
		{
			name:       "available greater than total disabled",
			snapshot:   FilesystemSnapshot{TotalBytes: 1000, AvailableBytes: 1001},
			percentage: 85,
			want:       false,
		},
		{
			name:       "large filesystem does not overflow",
			snapshot:   FilesystemSnapshot{TotalBytes: maxUint64, AvailableBytes: maxUint64 / 10},
			percentage: 85,
			want:       true,
		},
		{
			name:       "large filesystem below high threshold",
			snapshot:   FilesystemSnapshot{TotalBytes: maxUint64, AvailableBytes: maxUint64 / 10},
			percentage: 95,
			want:       false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := UsageAtLeastPercent(test.snapshot, test.percentage); got != test.want {
				t.Fatalf("UsageAtLeastPercent(%+v, %d) = %v, want %v", test.snapshot, test.percentage, got, test.want)
			}
		})
	}
}
