//go:build unit

package service

import (
	"fmt"
	"testing"
)

var supportDecisionBenchmarkResult SupportDecisionResult
var supportDecisionBenchmarkTable *SupportDecisionTable

func BenchmarkSupportDecisionHotLookup(b *testing.B) {
	for _, count := range []int{100, 10000, 30000} {
		b.Run(fmt.Sprintf("accounts_%d", count), func(b *testing.B) {
			table := benchmarkSupportDecisionTable(b, count, 1, 0)
			query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot-0"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				supportDecisionBenchmarkResult = table.Lookup(query)
			}
		})
	}
}

func BenchmarkSupportDecisionExactFallback(b *testing.B) {
	benchmarkSupportDecisionLookupByAccountCount(b, 0, "exact-99")
}

func BenchmarkSupportDecisionExactFallbackMiss(b *testing.B) {
	benchmarkSupportDecisionLookupByAccountCount(b, 0, "complete-exact-miss")
}

func BenchmarkSupportDecisionUnseenFallback(b *testing.B) {
	benchmarkSupportDecisionLookupByAccountCount(b, 0, "completely-unseen")
}

func BenchmarkSupportDecisionWildcardFallback256(b *testing.B) {
	benchmarkSupportDecisionLookupByAccountCount(b, SupportDecisionWildcardLimit, "complete-wildcard-miss")
}

func benchmarkSupportDecisionLookupByAccountCount(b *testing.B, wildcards int, model string) {
	for _, count := range []int{100, 10000, 30000} {
		b.Run(fmt.Sprintf("accounts_%d", count), func(b *testing.B) {
			table := benchmarkSupportDecisionTable(b, count, 1, wildcards)
			query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: model}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				supportDecisionBenchmarkResult = table.Lookup(query)
			}
		})
	}
}

func BenchmarkSupportDecisionOpenAIOAuthNormalizationFallback(b *testing.B) {
	for _, count := range []int{100, 10000, 30000} {
		b.Run(fmt.Sprintf("accounts_%d", count), func(b *testing.B) {
			accounts := make([]Account, count)
			for i := range accounts {
				accounts[i] = Account{ID: int64(i + 1), Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			}
			table := buildSupportDecisionTestTable(b, supportDecisionTestSnapshot(accounts, PlatformOpenAI, []string{"hot"}))
			query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}, RequestedModel: "openai/gpt-5-codex"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				supportDecisionBenchmarkResult = table.Lookup(query)
			}
		})
	}
}

func BenchmarkSupportDecisionAllowAllFallback(b *testing.B) {
	for _, count := range []int{100, 10000, 30000} {
		b.Run(fmt.Sprintf("accounts_%d", count), func(b *testing.B) {
			accounts := make([]Account, count)
			for i := range accounts {
				accounts[i] = Account{ID: int64(i + 1), Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
			}
			table := buildSupportDecisionTestTable(b, supportDecisionTestSnapshot(accounts, PlatformAnthropic, []string{"hot"}))
			query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "unseen"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				supportDecisionBenchmarkResult = table.Lookup(query)
			}
		})
	}
}

func BenchmarkSupportDecisionBuild(b *testing.B) {
	for _, count := range []int{100, 10000, 30000} {
		b.Run(fmt.Sprintf("accounts_%d", count), func(b *testing.B) {
			snapshot := benchmarkSupportDecisionSnapshot(count, 1, 0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				table, err := BuildSupportDecisionTable(snapshot, SupportDecisionBuildOptions{Generation: uint64(i + 1)})
				if err != nil {
					b.Fatal(err)
				}
				supportDecisionBenchmarkTable = table
			}
		})
	}
	b.Run("accounts_30000_hot_11_wildcards_256", func(b *testing.B) {
		snapshot := benchmarkSupportDecisionSnapshot(30000, 11, SupportDecisionWildcardLimit)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			table, err := BuildSupportDecisionTable(snapshot, SupportDecisionBuildOptions{Generation: uint64(i + 1)})
			if err != nil {
				b.Fatal(err)
			}
			supportDecisionBenchmarkTable = table
		}
	})
}

func benchmarkSupportDecisionTable(tb testing.TB, accounts, hot, wildcards int) *SupportDecisionTable {
	tb.Helper()
	table, err := BuildSupportDecisionTable(benchmarkSupportDecisionSnapshot(accounts, hot, wildcards), SupportDecisionBuildOptions{Generation: 1})
	if err != nil {
		tb.Fatal(err)
	}
	return table
}

func benchmarkSupportDecisionSnapshot(accountCount, hotCount, wildcardCount int) *SupportDecisionConstructionSnapshot {
	hot := make([]string, hotCount)
	for i := range hot {
		hot[i] = fmt.Sprintf("hot-%d", i)
	}
	accounts := make([]Account, accountCount)
	for i := range accounts {
		mapping := map[string]any{fmt.Sprintf("exact-%d", i%100): "upstream"}
		if i == 0 {
			for wildcard := 0; wildcard < wildcardCount; wildcard++ {
				mapping[fmt.Sprintf("wild-%03d-*", wildcard)] = "upstream"
			}
		}
		accounts[i] = Account{ID: int64(i + 1), Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": mapping}}
	}
	return supportDecisionTestSnapshot(accounts, PlatformAnthropic, hot)
}
