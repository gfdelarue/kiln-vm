package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestFormatMachineMemoryDistinguishesUnavailableFromMeasuredZero(t *testing.T) {
	unavailable := formatMachineMemory(models.MachineMetrics{
		MemoryLimitBytes: 3 * 1024 * 1024 * 1024,
		MemoryStatus:     "unavailable",
	})
	if unavailable != "unavailable/3.0GiB" {
		t.Fatalf("unavailable memory = %q, want unavailable/3.0GiB", unavailable)
	}

	measuredZero := formatMachineMemory(models.MachineMetrics{
		MemoryObserved:   true,
		MemoryStatus:     "measured",
		MemoryUsedBytes:  0,
		MemoryLimitBytes: 3 * 1024 * 1024 * 1024,
	})
	if measuredZero != "0B/3.0GiB" {
		t.Fatalf("measured zero memory = %q, want 0B/3.0GiB", measuredZero)
	}
}

func TestPrintNodeHealthExplainsGatewayDNSWarning(t *testing.T) {
	output := captureStdout(t, func() {
		printNodeHealth(models.NodeMetrics{
			Status: "warn",
			Warnings: []models.NodeWarning{{
				Code:      "gateway_dns_degraded",
				Reason:    "tap_ownership_collision",
				MachineID: "machine-api",
				Machine:   "api-vm",
				Message:   "physical-host TAP identity collides with an unowned link; inspect the conflicting link",
			}},
		})
	})

	for _, want := range []string{
		"Node: warn",
		"Warning [gateway_dns_degraded]",
		"machine=api-vm",
		"reason=tap_ownership_collision",
		"message=physical-host TAP identity collides with an unowned link; inspect the conflicting link",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("node health output missing %q:\n%s", want, output)
		}
	}
}

func TestPrintNodeHealthBoundsWarningFields(t *testing.T) {
	longMessage := "first line\n" + strings.Repeat("x", maxGatewayDNSErrorDisplayRunes+40)
	output := captureStdout(t, func() {
		printNodeHealth(models.NodeMetrics{
			Status: "warn",
			Warnings: []models.NodeWarning{{
				Code:      "gateway_dns_degraded\nforged-line",
				Reason:    strings.Repeat("r", 100),
				MachineID: "machine-api",
				Message:   longMessage,
			}},
		})
	})

	if strings.Contains(output, "\nforged-line") || strings.Contains(output, "first line\n") {
		t.Fatalf("node warning output retained an injected line break:\n%s", output)
	}
	if !strings.Contains(output, "gateway_dns_degraded forged-line") || !strings.Contains(output, "...") {
		t.Fatalf("node warning output was not flattened and visibly bounded:\n%s", output)
	}
}

func TestPrintVMMetricsRendersGatewayDNSOperatorStatus(t *testing.T) {
	checkedAt := time.Date(2026, 7, 21, 10, 11, 12, 0, time.FixedZone("BST", 60*60))
	successAt := checkedAt.Add(-2 * time.Minute)
	output := captureStdout(t, func() {
		printVMMetrics(models.VMMetrics{Machine: models.MachineMetrics{
			MachineID:    "machine-dns",
			Name:         "api-vm",
			State:        models.MachineStateRunning,
			HealthStatus: "degraded",
			GatewayDNS: models.GatewayDNSStatus{
				Status:         "degraded",
				Reason:         "admission_missing",
				GatewayDNSPath: true,
				ExternalDNS:    false,
				DesiredRules:   2,
				InstalledRules: 1,
				InFlight:       3,
				LastCheckedAt:  &checkedAt,
				LastSuccessAt:  &successAt,
				LastError:      "gateway DNS admission rules are incomplete",
			},
		}})
	})

	for _, want := range []string{
		"Gateway DNS [api-vm]",
		"status=degraded",
		"reason=admission_missing",
		"path=yes",
		"external=no",
		"rules=1/2",
		"in_flight=3",
		"checked=2026-07-21T09:11:12Z",
		"success=2026-07-21T09:09:12Z",
		"error=gateway DNS admission rules are incomplete",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, output)
		}
	}
}

func TestPrintMetricsSummaryRendersGatewayDNSForEachMachine(t *testing.T) {
	output := captureStdout(t, func() {
		printMetricsSummary(models.MetricsSummary{
			Node: models.NodeMetrics{Status: "warn"},
			Machines: []models.MachineMetrics{
				{
					MachineID: "machine-api",
					Name:      "api-vm",
					GatewayDNS: models.GatewayDNSStatus{
						Status:       "ready",
						DesiredRules: 2,
					},
				},
				{
					MachineID: "machine-worker",
					Name:      "worker-vm",
					GatewayDNS: models.GatewayDNSStatus{
						Status:       "unknown",
						Reason:       "not_applicable",
						DesiredRules: 0,
					},
				},
			},
		})
	})

	for _, want := range []string{
		"Gateway DNS [api-vm]: status=ready reason=-",
		"Gateway DNS [worker-vm]: status=unknown reason=not_applicable",
		"checked=- success=- error=-",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("summary output missing %q:\n%s", want, output)
		}
	}
}

func TestPrintVMMetricsRendersEmbeddedMachineGatewayDNS(t *testing.T) {
	output := captureStdout(t, func() {
		printVMMetrics(models.VMMetrics{
			Name: "frontend",
			Machine: models.MachineMetrics{
				MachineID: "machine-frontend",
				Name:      "frontend-machine",
				GatewayDNS: models.GatewayDNSStatus{
					Status:       "degraded",
					Reason:       "path_failed",
					DesiredRules: 2,
				},
			},
		})
	})

	if !strings.Contains(output, "Gateway DNS [frontend-machine]: status=degraded reason=path_failed") {
		t.Fatalf("VM metrics omitted embedded machine Gateway DNS status:\n%s", output)
	}
}

func TestFormatGatewayDNSErrorIsSingleLineAndBounded(t *testing.T) {
	input := "first line\n" + strings.Repeat("x", maxGatewayDNSErrorDisplayRunes+40)
	got := formatGatewayDNSError(input)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("formatted Gateway DNS error contains a line break: %q", got)
	}
	if len([]rune(got)) != maxGatewayDNSErrorDisplayRunes {
		t.Fatalf("formatted Gateway DNS error length = %d, want %d: %q", len([]rune(got)), maxGatewayDNSErrorDisplayRunes, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("formatted Gateway DNS error was not visibly truncated: %q", got)
	}
	if got := formatGatewayDNSError(" \n\t "); got != "-" {
		t.Fatalf("blank Gateway DNS error = %q, want -", got)
	}
}
