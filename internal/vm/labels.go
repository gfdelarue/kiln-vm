package vm

import (
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

const (
	MaxLabels          = 32
	MaxLabelValueBytes = 255
)

var labelKeyPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]{0,61}[a-z0-9])?$`)

type LabelSelector struct {
	Key      string
	Value    string
	HasValue bool
}

func ValidateLabels(field string, labels map[string]string) error {
	if len(labels) > MaxLabels {
		return &apierr.ValidationError{Field: field, Message: fmt.Sprintf("must have at most %d labels", MaxLabels)}
	}
	for key, value := range labels {
		if !labelKeyPattern.MatchString(key) {
			return &apierr.ValidationError{Field: field, Message: fmt.Sprintf("key %q must be 1 to 63 characters of lowercase letters, digits, '.', '_', '/' or '-', starting and ending with a letter or digit", key)}
		}
		if len(value) > MaxLabelValueBytes {
			return &apierr.ValidationError{Field: field, Message: fmt.Sprintf("value for %q must be at most %d bytes", key, MaxLabelValueBytes)}
		}
	}
	return nil
}

func normalizeLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	return maps.Clone(labels)
}

func ParseLabelSelectors(raw []string) ([]LabelSelector, error) {
	selectors := make([]LabelSelector, 0, len(raw))
	for _, item := range raw {
		key, value, hasValue := strings.Cut(item, "=")
		if !labelKeyPattern.MatchString(key) {
			return nil, &apierr.ValidationError{Field: "label", Message: fmt.Sprintf("selector %q must be key or key=value with a valid label key", item)}
		}
		if len(value) > MaxLabelValueBytes {
			return nil, &apierr.ValidationError{Field: "label", Message: fmt.Sprintf("selector value for %q must be at most %d bytes", key, MaxLabelValueBytes)}
		}
		selectors = append(selectors, LabelSelector{Key: key, Value: value, HasValue: hasValue})
	}
	return selectors, nil
}

func MatchLabels(labels map[string]string, selectors []LabelSelector) bool {
	for _, selector := range selectors {
		value, ok := labels[selector.Key]
		if !ok || (selector.HasValue && value != selector.Value) {
			return false
		}
	}
	return true
}
