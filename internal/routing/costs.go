package routing

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// CostTable holds per-model pricing data.
type CostTable struct {
	Models map[string]ModelCost `yaml:"models"`
}

// ModelCost holds pricing per million tokens.
type ModelCost struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

// EffectiveCost returns a blended cost assuming the project's 15:1 output:input ratio.
func (mc ModelCost) EffectiveCost() float64 {
	return mc.InputPerMTok + 15.0*mc.OutputPerMTok
}

// LoadCostTable loads a costs.yaml file.
func LoadCostTable(path string) (*CostTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read costs.yaml: %w", err)
	}
	var table CostTable
	if err := yaml.Unmarshal(data, &table); err != nil {
		return nil, fmt.Errorf("parse costs.yaml: %w", err)
	}
	if table.Models == nil {
		table.Models = map[string]ModelCost{}
	}
	return &table, nil
}

// CheapestCapable returns the model with the lowest effective cost from the capable list.
func (ct *CostTable) CheapestCapable(capable []string) string {
	if len(capable) == 0 {
		return ""
	}
	best := ""
	bestCost := -1.0
	for _, model := range capable {
		mc, ok := ct.Models[model]
		if !ok {
			continue
		}
		cost := mc.EffectiveCost()
		if bestCost < 0 || cost < bestCost {
			best = model
			bestCost = cost
		}
	}
	return best
}
