package archivers

// Completeness states for a stored social capture.
//
// The archive contract is that a capture may read fulfilled only when every
// obtainable source asset was stored. That is a claim about media Arker never
// saw, so it can only be made when the extractor told us how many assets the
// post has. Three states keep the claim honest:
//
//   - complete: the expected asset count is known and every one was stored.
//   - partial:  the expected count is known and some assets are missing, or the
//     extractor reported a failure while still producing output.
//   - unknown:  nothing said how many assets the post has, so "all of them" is
//     unprovable. Unknown is not a soft complete; it never reads green.
//
// An empty string is a fourth, implicit state: a row written before
// completeness was tracked. It must be treated as unknown, never as complete —
// the absence of evidence is not evidence of a full capture.
const (
	CompletenessComplete = "complete"
	CompletenessPartial  = "partial"
	CompletenessUnknown  = "unknown"
)

// maxCompletenessMissingIndices caps how many missing slide numbers are
// recorded. A post that lost 500 slides is described just as well by the first
// few plus the counts, and the record is stored in every archive's metadata.
const maxCompletenessMissingIndices = 50

// Completeness is the durable record of how much of a post a capture stored.
// It is written into the archive's normalized metadata so the answer survives
// independently of the database, and its state is mirrored onto the archive
// item so the API can answer without opening the artifact.
type Completeness struct {
	State string `json:"state"`
	// Expected is the asset count the extractor reported. Nil means the
	// extractor did not say, which forces the unknown state.
	Expected *int `json:"expected,omitempty"`
	Stored   int  `json:"stored"`
	// MissingIndices lists the 1-based slide numbers that were expected but not
	// stored, when they can be derived from the stored filenames.
	MissingIndices []int `json:"missing_indices,omitempty"`
}

// NormalizeCompletenessState maps a stored value onto a known state. Anything
// unrecognized — including the empty string written before this was tracked —
// becomes unknown, so a corrupt or legacy value can never read as complete.
func NormalizeCompletenessState(state string) string {
	switch state {
	case CompletenessComplete, CompletenessPartial, CompletenessUnknown:
		return state
	default:
		return CompletenessUnknown
	}
}

// CompletenessFromCounts derives the state from what the extractor promised and
// what actually landed on disk.
//
// runFailed reports whether the extractor exited with an error. It only matters
// when the expected count is unknown: an extractor that failed and still
// produced files definitely lost something, which is more specific than
// unknown. When the count is known it is the only thing that decides the state,
// so a run that exits non-zero for an unrelated reason but stored every asset
// is still complete.
func CompletenessFromCounts(expected *int, stored int, runFailed bool) Completeness {
	result := Completeness{Stored: stored}
	if expected != nil && *expected > 0 {
		result.Expected = expected
		if stored >= *expected {
			result.State = CompletenessComplete
		} else {
			result.State = CompletenessPartial
		}
		return result
	}
	if runFailed {
		result.State = CompletenessPartial
		return result
	}
	result.State = CompletenessUnknown
	return result
}
