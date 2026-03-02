// Package routing implements cost-aware capability matching for model/agent selection.
// It unifies the routing logic previously split across lib-routing.sh, agent-roles.yaml,
// and interserve classify into a single compiled Go package.
package routing

// ModelTier represents Claude model capability tiers.
type ModelTier int

const (
	TierUnknown ModelTier = 0
	TierHaiku   ModelTier = 1
	TierSonnet  ModelTier = 2
	TierOpus    ModelTier = 3
)

// ParseModelTier converts a model name string to a tier.
func ParseModelTier(s string) ModelTier {
	switch s {
	case "haiku":
		return TierHaiku
	case "sonnet":
		return TierSonnet
	case "opus":
		return TierOpus
	default:
		return TierUnknown
	}
}

func (t ModelTier) String() string {
	switch t {
	case TierHaiku:
		return "haiku"
	case TierSonnet:
		return "sonnet"
	case TierOpus:
		return "opus"
	default:
		return "unknown"
	}
}
