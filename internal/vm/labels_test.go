package vm

import (
	"errors"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

func TestValidateLabels(t *testing.T) {
	valid := []map[string]string{
		nil,
		{"a": ""},
		{"kiln-run/env": "abc", "app.kubernetes.io_name": strings.Repeat("v", MaxLabelValueBytes)},
		{strings.Repeat("k", 63): "v"},
		manyLabels(MaxLabels),
	}
	for _, labels := range valid {
		if err := ValidateLabels("labels", labels); err != nil {
			t.Errorf("ValidateLabels(%v) = %v, want nil", labels, err)
		}
	}

	invalid := []map[string]string{
		{"": "v"},
		{"Upper": "v"},
		{"-leading": "v"},
		{"trailing/": "v"},
		{"has space": "v"},
		{strings.Repeat("k", 64): "v"},
		{"k": strings.Repeat("v", MaxLabelValueBytes+1)},
		manyLabels(MaxLabels + 1),
	}
	for _, labels := range invalid {
		err := ValidateLabels("labels", labels)
		var validation *apierr.ValidationError
		if !errors.As(err, &validation) || validation.Field != "labels" || !errors.Is(err, apierr.ErrValidation) {
			t.Errorf("ValidateLabels(%d labels) = %v, want validation error on labels", len(labels), err)
		}
	}
}

func TestParseLabelSelectorsAndMatch(t *testing.T) {
	selectors, err := ParseLabelSelectors([]string{"owner=kiln-run", "env", "empty="})
	if err != nil {
		t.Fatalf("ParseLabelSelectors: %v", err)
	}
	want := []LabelSelector{
		{Key: "owner", Value: "kiln-run", HasValue: true},
		{Key: "env"},
		{Key: "empty", HasValue: true},
	}
	if len(selectors) != len(want) {
		t.Fatalf("selectors = %+v", selectors)
	}
	for i := range want {
		if selectors[i] != want[i] {
			t.Fatalf("selector %d = %+v, want %+v", i, selectors[i], want[i])
		}
	}

	matches := []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"owner": "kiln-run", "env": "x", "empty": ""}, true},
		{map[string]string{"owner": "kiln-run", "env": "", "empty": "", "extra": "y"}, true},
		{map[string]string{"owner": "other", "env": "x", "empty": ""}, false},
		{map[string]string{"owner": "kiln-run", "empty": ""}, false},
		{map[string]string{"owner": "kiln-run", "env": "x", "empty": "set"}, false},
		{nil, false},
	}
	for _, tt := range matches {
		if got := MatchLabels(tt.labels, selectors); got != tt.want {
			t.Errorf("MatchLabels(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
	if !MatchLabels(nil, nil) {
		t.Fatal("MatchLabels with no selectors must match")
	}

	for _, raw := range []string{"", "=v", "Bad=v", "k=" + strings.Repeat("v", MaxLabelValueBytes+1)} {
		_, err := ParseLabelSelectors([]string{raw})
		var validation *apierr.ValidationError
		if !errors.As(err, &validation) || validation.Field != "label" {
			t.Errorf("ParseLabelSelectors(%q) = %v, want validation error on label", raw, err)
		}
	}
}
