package walk

import (
	"path"
	"strings"
)

// translationExts are translation catalog formats wherever they are:
// gettext, XLIFF, Flutter ARB and Apple strings files.
var translationExts = map[string]bool{".po": true, ".pot": true, ".xliff": true, ".xlf": true, ".arb": true, ".strings": true, ".stringsdict": true}

// translationDirs hold one data file per language (de.json, fr.yml, …).
// "language" and "languages" are left out: they often hold code or grammars.
var translationDirs = map[string]bool{"locale": true, "locales": true, "i18n": true, "l10n": true, "translations": true, "translation": true, "lang": true, "langs": true}

// translationDataExts are the data formats a translation folder uses.
var translationDataExts = map[string]bool{".json": true, ".yaml": true, ".yml": true, ".toml": true, ".properties": true, ".xml": true}

// IsTranslation reports whether a slash path is a translation catalog: the
// same strings in every language, which never say what the code does. In
// miniflux they were 43% of the chunks a scan sent, and only ever ranked
// below the code a question was after.
func IsTranslation(rel string) bool {
	dir, name := path.Split(rel)
	ext := strings.ToLower(path.Ext(name))
	if translationExts[ext] {
		return true
	}
	if !translationDataExts[ext] {
		return false
	}
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if translationDirs[strings.ToLower(part)] {
			return true
		}
	}
	return false
}
