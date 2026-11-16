package refid

import (
	"sort"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

const (
	DefaultDisplayLength = 8
	MinimumPrefixLength  = 4
)

type Candidate struct {
	ID    string
	Label string
}

func Normalize(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

func AbbreviateAll(ids []string, minLen int) map[string]string {
	if minLen <= 0 {
		minLen = DefaultDisplayLength
	}

	normalized := make(map[string]string, len(ids))
	for _, id := range ids {
		normalized[id] = Normalize(id)
	}

	result := make(map[string]string, len(ids))
	for _, id := range ids {
		norm := normalized[id]
		if len(norm) <= minLen {
			result[id] = id
			continue
		}

		length := minLen
		for ; length < len(norm); length++ {
			prefix := norm[:length]
			unique := true
			for _, otherID := range ids {
				if otherID == id {
					continue
				}
				if strings.HasPrefix(normalized[otherID], prefix) {
					unique = false
					break
				}
			}
			if unique {
				break
			}
		}
		if length > len(norm) {
			length = len(norm)
		}
		result[id] = norm[:length]
	}
	return result
}

func Resolve(resource, ref string, candidates []Candidate) (Candidate, error) {
	ref = strings.TrimSpace(ref)
	normalizedRef := Normalize(ref)

	for _, candidate := range candidates {
		if strings.EqualFold(candidate.ID, ref) || Normalize(candidate.ID) == normalizedRef {
			return candidate, nil
		}
	}

	if len(normalizedRef) < MinimumPrefixLength {
		return Candidate{}, &apierr.ValidationError{
			Field:   resource + "_id",
			Message: "ID prefix must be at least 4 characters",
		}
	}

	var matches []Candidate
	for _, candidate := range candidates {
		if strings.HasPrefix(Normalize(candidate.ID), normalizedRef) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return Candidate{}, &apierr.NotFoundError{Resource: resource, ID: ref}
	}
	if len(matches) > 1 {
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].ID < matches[j].ID
		})
		matchIDs := make([]string, 0, len(matches))
		for _, match := range matches {
			if match.Label != "" {
				matchIDs = append(matchIDs, match.ID+" ("+match.Label+")")
			} else {
				matchIDs = append(matchIDs, match.ID)
			}
		}
		return Candidate{}, &apierr.AmbiguousReferenceError{
			Resource: resource,
			Ref:      ref,
			Matches:  matchIDs,
		}
	}
	return matches[0], nil
}
