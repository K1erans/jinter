package walk

import (
	"path"
	"strings"
)

// docExts are prose formats. Docs describe behaviour too, so like tests they
// match questions and crowd the code out of the results; Markdown alone was
// 11% of the tokens a scan of one large repo would send.
var docExts = map[string]bool{".md": true, ".mdx": true, ".markdown": true, ".rst": true, ".adoc": true, ".asciidoc": true}

// IsDoc reports whether a slash path is documentation.
func IsDoc(rel string) bool {
	return docExts[strings.ToLower(path.Ext(rel))]
}
