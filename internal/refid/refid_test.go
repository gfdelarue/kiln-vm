package refid

import (
	"errors"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

func TestAbbreviateAllUsesShortestUniqueHyphenlessPrefix(t *testing.T) {
	ids := []string{
		"20f805c1-7be1-4e8e-94c6-ff5cda2a14c5",
		"20f805c1-ffff-4e8e-94c6-ff5cda2a14c5",
		"d4223daa-11dc-4f65-8b87-05675d4d7fba",
	}

	got := AbbreviateAll(ids, DefaultDisplayLength)

	if got[ids[0]] != "20f805c17" {
		t.Fatalf("first abbreviation = %q, want %q", got[ids[0]], "20f805c17")
	}
	if got[ids[1]] != "20f805c1f" {
		t.Fatalf("second abbreviation = %q, want %q", got[ids[1]], "20f805c1f")
	}
	if got[ids[2]] != "d4223daa" {
		t.Fatalf("third abbreviation = %q, want %q", got[ids[2]], "d4223daa")
	}
}

func TestResolveAcceptsFullHyphenlessAndUniquePrefix(t *testing.T) {
	candidates := []Candidate{
		{ID: "20f805c1-7be1-4e8e-94c6-ff5cda2a14c5", Label: "first"},
		{ID: "d4223daa-11dc-4f65-8b87-05675d4d7fba", Label: "second"},
	}

	got, err := Resolve("machine", "20f805c17be14e8e94c6ff5cda2a14c5", candidates)
	if err != nil {
		t.Fatalf("Resolve full hyphenless: %v", err)
	}
	if got.ID != candidates[0].ID {
		t.Fatalf("full hyphenless resolved %q, want %q", got.ID, candidates[0].ID)
	}

	got, err = Resolve("machine", "d4223daa", candidates)
	if err != nil {
		t.Fatalf("Resolve prefix: %v", err)
	}
	if got.ID != candidates[1].ID {
		t.Fatalf("prefix resolved %q, want %q", got.ID, candidates[1].ID)
	}
}

func TestResolveRejectsAmbiguousPrefix(t *testing.T) {
	candidates := []Candidate{
		{ID: "20f805c1-7be1-4e8e-94c6-ff5cda2a14c5", Label: "first"},
		{ID: "20f805c1-ffff-4e8e-94c6-ff5cda2a14c5", Label: "second"},
	}

	_, err := Resolve("machine", "20f805c1", candidates)
	if err == nil {
		t.Fatal("expected ambiguous prefix error")
	}
	var ambiguous *apierr.AmbiguousReferenceError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %T %v, want AmbiguousReferenceError", err, err)
	}
	if !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("error does not unwrap to conflict: %v", err)
	}
}

func TestResolveRejectsTooShortNonExactPrefix(t *testing.T) {
	candidates := []Candidate{{ID: "20f805c1-7be1-4e8e-94c6-ff5cda2a14c5"}}

	_, err := Resolve("machine", "20f", candidates)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, apierr.ErrValidation) {
		t.Fatalf("error does not unwrap to validation: %v", err)
	}
}
