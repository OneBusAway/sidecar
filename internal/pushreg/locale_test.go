package pushreg_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/pushreg"
)

func TestNormalizeLocale(t *testing.T) {
	t.Parallel()

	catalog := []string{"es", "zh-Hans", "zh-Hant", "tl", "pt", "fr-CA"}
	tests := []struct {
		tag, want string
	}{
		{"es", "es"},
		{"ES", "es"},         // exact match is case-insensitive
		{"fr-ca", "fr-CA"},   // returns the catalog's spelling
		{"zh-CN", "zh-Hans"}, // alias
		{"zh-SG", "zh-Hans"}, // alias
		{"zh-TW", "zh-Hant"}, // alias
		{"zh-HK", "zh-Hant"}, // alias
		{"fil", "tl"},        // alias
		{"pt-BR", "pt"},      // alias
		{"es-MX", "es"},      // bare primary subtag
		{"fr-FR", ""},        // primary subtag fr not in catalog (only fr-CA)
		{"de", ""},           // no match at all -> English copy
		{"", ""},
		{"  es  ", "es"}, // trimmed
	}
	for _, tt := range tests {
		if got := pushreg.NormalizeLocale(tt.tag, catalog); got != tt.want {
			t.Errorf("NormalizeLocale(%q) = %q; want %q", tt.tag, got, tt.want)
		}
	}
}
