//go:build system

package systemtest

import (
	"errors"
	"testing"
)

func TestNetworkLinkInspectionDistinguishesMissingFromFailure(t *testing.T) {
	missing, err := linkExistsFromResultForTest("kbrmissing", []byte(`Device "kbrmissing" does not exist.`), errors.New("exit status 1"))
	if err != nil || missing {
		t.Fatalf("missing link result = (%t, %v), want (false, nil)", missing, err)
	}

	marker := errors.New("permission denied")
	exists, err := linkExistsFromResultForTest("kbrdenied", []byte("permission denied"), marker)
	if exists || !errors.Is(err, marker) {
		t.Fatalf("failed link inspection result = (%t, %v), want propagated error", exists, err)
	}
}
