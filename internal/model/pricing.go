package model

// Price is what a model charges, in USD per million tokens of each kind.
type Price struct {
	Input      float64 `json:"input,omitempty" yaml:"input,omitempty"`
	Output     float64 `json:"output,omitempty" yaml:"output,omitempty"`
	CacheRead  float64 `json:"cacheRead,omitempty" yaml:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty" yaml:"cacheWrite,omitempty"`
}

// Cost is what u costs at p.
func (p Price) Cost(u Usage) float64 {
	return (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
}

// Pricing is the price of each model, keyed by model id in the provider's
// namespace.
type Pricing map[string]Price

// cost is what u costs on modelID, zero for a model without a price.
func (p Pricing) cost(modelID string, u Usage) float64 { return p[modelID].Cost(u) }
