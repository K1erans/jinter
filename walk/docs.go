package walk

import "path"

// IsDoc reports whether a slash path is documentation.
func IsDoc(rel string) bool {
	ext := path.Ext(rel)
	switch len(ext) {
	case 3:
		return asciiEqualFold(ext, ".md")
	case 4:
		return asciiEqualFold(ext, ".mdx") || asciiEqualFold(ext, ".rst")
	case 5:
		return asciiEqualFold(ext, ".adoc")
	case 9:
		return asciiEqualFold(ext, ".markdown") || asciiEqualFold(ext, ".asciidoc")
	}
	return false
}
