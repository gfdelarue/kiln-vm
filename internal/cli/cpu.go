package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func parseCPUFlag(raw string, kind string) (*models.CPUConfig, error) {
	raw = strings.TrimSpace(raw)
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "shared"
	}
	if raw == "" {
		if kind == "pooled" {
			return &models.CPUConfig{Kind: kind}, nil
		}
		return nil, nil
	}

	var millis int
	if strings.HasSuffix(raw, "m") {
		value := strings.TrimSuffix(raw, "m")
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("invalid CPU value %q", raw)
		}
		millis = parsed
	} else {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid CPU value %q", raw)
		}
		millis = int(math.Round(parsed * 1000))
	}

	return &models.CPUConfig{Kind: kind, Millis: millis}, nil
}

func formatCPU(cpu models.CPUConfig) string {
	kind := cpu.Kind
	if kind == "" {
		kind = "shared"
	}
	millis := cpu.Millis
	if millis <= 0 && cpu.VCPUs > 0 {
		millis = cpu.VCPUs * 1000
	}
	if millis <= 0 {
		return kind
	}
	if millis%1000 == 0 {
		return fmt.Sprintf("%s %d", kind, millis/1000)
	}
	return fmt.Sprintf("%s %dm", kind, millis)
}
