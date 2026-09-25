package model

import (
	"math"
	"testing"
)

func TestPriceCost(t *testing.T) {
	sonnet := Price{Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75}
	tests := []struct {
		name  string
		price Price
		usage Usage
		want  float64
	}{
		{"zero usage", sonnet, Usage{}, 0},
		{"zero price", Price{}, Usage{Input: 1_000_000, Output: 1_000_000}, 0},
		{"input only", sonnet, Usage{Input: 1_000_000}, 3.0},
		{"cache read only", sonnet, Usage{CacheRead: 500_000}, 0.15},
		{"all four buckets", sonnet, Usage{Input: 1_000_000, CacheRead: 500_000, CacheWrite: 200_000, Output: 100_000}, 3.0 + 0.15 + 0.75 + 1.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.price.Cost(tt.usage); math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("Cost = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	a := Usage{Input: 10, CacheRead: 20, CacheWrite: 30, Output: 5}
	b := Usage{Input: 1, CacheRead: 2, CacheWrite: 3, Output: 4}
	if got := a.Add(b); got != (Usage{Input: 11, CacheRead: 22, CacheWrite: 33, Output: 9}) {
		t.Fatalf("Add = %+v", got)
	}
	if got := a.Prompt(); got != 60 {
		t.Fatalf("Prompt = %d, want input plus both cached parts", got)
	}
}

func TestEnumsValid(t *testing.T) {
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"openrouter", ProviderOpenRouter.Valid(), true},
		{"openai", ProviderOpenAI.Valid(), true},
		{"anthropic", ProviderAnthropic.Valid(), true},
		{"unknown provider", ProviderType("cohere").Valid(), false},
		{"empty provider", ProviderType("").Valid(), false},
		{"user", RoleUser.Valid(), true},
		{"assistant", RoleAssistant.Valid(), true},
		{"system is not a message role", Role("system").Valid(), false},
		{"tool choice tool", ToolChoiceTool.Valid(), true},
		{"tool choice none", ToolChoiceMode("none").Valid(), false},
		{"stop other", StopOther.Valid(), true},
		{"stop unknown", StopReason("refusal").Valid(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("Valid = %v, want %v", tt.got, tt.want)
			}
		})
	}
}
