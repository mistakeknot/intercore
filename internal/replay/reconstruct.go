package replay

import (
	"fmt"

	"github.com/mistakeknot/intercore/internal/event"
)

// Decision is a deterministic replay step reconstructed from the event stream.
type Decision struct {
	EventID      int64    `json:"event_id"`
	Source       string   `json:"source"`
	Type         string   `json:"type"`
	FromState    string   `json:"from_state"`
	ToState      string   `json:"to_state"`
	Reason       string   `json:"reason,omitempty"`
	Timestamp    int64    `json:"timestamp"`
	InputIDs     []int64  `json:"input_ids,omitempty"`
	ArtifactRefs []string `json:"artifact_refs,omitempty"`
}

// BuildTimeline deterministically reconstructs phase/dispatch decisions and links
// them to recorded nondeterministic inputs.
func BuildTimeline(events []event.Event, inputs []*Input) []Decision {
	inputsByEvent := make(map[string][]*Input)
	for _, in := range inputs {
		if in == nil || in.EventSource == "" || in.EventID == nil {
			continue
		}
		k := fmt.Sprintf("%s:%d", in.EventSource, *in.EventID)
		inputsByEvent[k] = append(inputsByEvent[k], in)
	}

	out := make([]Decision, 0, len(events))
	for _, e := range events {
		if e.Source != event.SourcePhase && e.Source != event.SourceDispatch {
			continue
		}
		d := Decision{
			EventID:   e.ID,
			Source:    e.Source,
			Type:      e.Type,
			FromState: e.FromState,
			ToState:   e.ToState,
			Reason:    e.Reason,
			Timestamp: e.Timestamp.Unix(),
		}
		if e.Envelope != nil {
			d.ArtifactRefs = append(d.ArtifactRefs, e.Envelope.InputArtifactRefs...)
			d.ArtifactRefs = append(d.ArtifactRefs, e.Envelope.OutputArtifactRefs...)
		}
		k := fmt.Sprintf("%s:%d", e.Source, e.ID)
		if ins := inputsByEvent[k]; len(ins) > 0 {
			for _, in := range ins {
				d.InputIDs = append(d.InputIDs, in.ID)
			}
		}
		out = append(out, d)
	}

	return out
}
