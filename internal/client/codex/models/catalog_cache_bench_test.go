package models

import "testing"

func BenchmarkLoadCodexClientModelTemplatesCached(b *testing.B) {
	if _, _, err := loadCodexClientModelTemplates(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := loadCodexClientModelTemplates(); err != nil {
			b.Fatal(err)
		}
	}
}
