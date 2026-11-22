package walk

import (
	"path"
	"strings"
)

// IsTranslation reports whether a slash path is a translation catalog: the
// same strings in every language, which never say what the code does. In
// miniflux they were 43% of the chunks a scan sent, and only ever ranked
// below the code a question was after.
func IsTranslation(rel string) bool {
	dir, name := splitSlash(rel)
	ext := path.Ext(name)
	if isTranslationExt(ext) {
		return true
	}
	if !isTranslationDataExt(ext) {
		return false
	}
	for s := strings.Trim(dir, "/"); s != ""; {
		part := s
		if i := strings.IndexByte(s, '/'); i >= 0 {
			part, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		if isTranslationDir(part) {
			return true
		}
	}
	return false
}

func isTranslationExt(ext string) bool {
	switch len(ext) {
	case 3:
		return asciiEqualFold(ext, ".po")
	case 4:
		return asciiEqualFold(ext, ".pot") || asciiEqualFold(ext, ".xlf") || asciiEqualFold(ext, ".arb")
	case 6:
		return asciiEqualFold(ext, ".xliff")
	case 8:
		return asciiEqualFold(ext, ".strings")
	case 12:
		return asciiEqualFold(ext, ".stringsdict")
	}
	return false
}

func isTranslationDataExt(ext string) bool {
	switch len(ext) {
	case 4:
		return asciiEqualFold(ext, ".yml") || asciiEqualFold(ext, ".xml")
	case 5:
		return asciiEqualFold(ext, ".json") || asciiEqualFold(ext, ".yaml") || asciiEqualFold(ext, ".toml")
	case 11:
		return asciiEqualFold(ext, ".properties")
	}
	return false
}

func isTranslationDir(part string) bool {
	switch len(part) {
	case 4:
		return asciiEqualFold(part, "i18n") || asciiEqualFold(part, "l10n") || asciiEqualFold(part, "lang")
	case 5:
		return asciiEqualFold(part, "langs")
	case 6:
		return asciiEqualFold(part, "locale")
	case 7:
		return asciiEqualFold(part, "locales")
	case 11:
		return asciiEqualFold(part, "translation")
	case 12:
		return asciiEqualFold(part, "translations")
	}
	return false
}
