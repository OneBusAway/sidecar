package pushreg

import "strings"

// localeAliases are the spec §4 tag aliases, applied after an exact catalog
// match fails and before falling back to the bare primary subtag.
var localeAliases = map[string]string{
	"zh-cn": "zh-Hans", "zh-sg": "zh-Hans",
	"zh-tw": "zh-Hant", "zh-hk": "zh-Hant",
	"fil": "tl", "pt-br": "pt",
}

// NormalizeLocale maps a reported BCP-47 tag onto catalog: exact
// case-insensitive match, then aliases, then bare primary subtag, else ""
// (meaning English copy). The returned value is the catalog's own spelling,
// so callers can key translation lookups on it directly.
func NormalizeLocale(tag string, catalog []string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	match := func(want string) string {
		for _, c := range catalog {
			if strings.EqualFold(c, want) {
				return c
			}
		}
		return ""
	}
	if m := match(tag); m != "" {
		return m
	}
	if alias, ok := localeAliases[strings.ToLower(tag)]; ok {
		if m := match(alias); m != "" {
			return m
		}
	}
	if primary, _, found := strings.Cut(tag, "-"); found {
		if m := match(primary); m != "" {
			return m
		}
	}
	return ""
}
