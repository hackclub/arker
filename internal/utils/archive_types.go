package utils

import "strings"

// Archive type identifiers. Tool-backed archivers are named after the tool that
// produces them so the provenance of a stored artifact is obvious from its type
// alone: a "yt-dlp" item came out of yt-dlp, a "gallery-dl" item out of
// gallery-dl. Browser- and protocol-backed archivers keep their format names.
const (
	ArchiveTypeMHTML      = "mhtml"
	ArchiveTypeScreenshot = "screenshot"
	ArchiveTypeGit        = "git"
	ArchiveTypeYtDlp      = "yt-dlp"
	ArchiveTypeGalleryDl  = "gallery-dl"
	ArchiveTypeItch       = "itch"
)

// canonicalArchiveTypes is the set of types the system creates today.
var canonicalArchiveTypes = []string{
	ArchiveTypeMHTML,
	ArchiveTypeScreenshot,
	ArchiveTypeGit,
	ArchiveTypeYtDlp,
	ArchiveTypeGalleryDl,
	ArchiveTypeItch,
}

// legacyArchiveTypeAliases maps retired type names to their canonical form.
//
// "youtube" was the original name for the yt-dlp archiver, back when YouTube
// was the only site it handled. It is still baked into archived permalinks,
// API clients, and every archive_items row written before the rename, so it
// has to keep resolving forever.
var legacyArchiveTypeAliases = map[string]string{
	"youtube": ArchiveTypeYtDlp,
}

// NormalizeArchiveType maps a possibly-legacy type name to its canonical form.
// Unknown values are returned unchanged so callers can reject them with a
// message naming what the caller actually sent.
func NormalizeArchiveType(archiveType string) string {
	trimmed := strings.TrimSpace(archiveType)
	if canonical, ok := legacyArchiveTypeAliases[strings.ToLower(trimmed)]; ok {
		return canonical
	}
	return trimmed
}

// NormalizeArchiveTypes normalizes a slice of type names, preserving order and
// dropping duplicates that collapse onto the same canonical name.
func NormalizeArchiveTypes(archiveTypes []string) []string {
	if archiveTypes == nil {
		return nil
	}
	seen := make(map[string]bool, len(archiveTypes))
	normalized := make([]string, 0, len(archiveTypes))
	for _, archiveType := range archiveTypes {
		canonical := NormalizeArchiveType(archiveType)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		normalized = append(normalized, canonical)
	}
	return normalized
}

// ArchiveTypeMatchValues returns every stored spelling that should resolve to
// the given type: its canonical name plus any retired alias.
//
// The rename migration is best-effort and runs once at startup, so a row can
// still hold a legacy type — during a rolling deploy, or if the migration
// failed. Querying with this instead of a bare equality keeps those rows
// reachable, which makes the migration an optimization rather than something
// correctness depends on.
func ArchiveTypeMatchValues(archiveType string) []string {
	canonical := NormalizeArchiveType(archiveType)
	values := []string{canonical}
	for legacy, target := range legacyArchiveTypeAliases {
		if target == canonical {
			values = append(values, legacy)
		}
	}
	return values
}

// ArchiveTypesEqual reports whether two stored type names refer to the same
// archiver, ignoring retired spellings.
func ArchiveTypesEqual(a, b string) bool {
	return NormalizeArchiveType(a) == NormalizeArchiveType(b)
}

// IsValidArchiveType reports whether a type name (canonical or legacy alias)
// names an archiver the system knows how to run.
func IsValidArchiveType(archiveType string) bool {
	canonical := NormalizeArchiveType(archiveType)
	for _, known := range canonicalArchiveTypes {
		if canonical == known {
			return true
		}
	}
	return false
}

// CanonicalArchiveTypes returns the archive types the system creates today.
func CanonicalArchiveTypes() []string {
	result := make([]string, len(canonicalArchiveTypes))
	copy(result, canonicalArchiveTypes)
	return result
}

// LegacyArchiveTypeAliases returns retired type name -> canonical name. Used by
// the startup migration and by tests that guard permalink compatibility.
func LegacyArchiveTypeAliases() map[string]string {
	result := make(map[string]string, len(legacyArchiveTypeAliases))
	for legacy, canonical := range legacyArchiveTypeAliases {
		result[legacy] = canonical
	}
	return result
}
