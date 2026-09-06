package dispatch

import "encoding/json"

// DispatchOutput is the public JSON contract for dispatch status, list, poll,
// and wait. It deliberately excludes internal storage fields. Optional values
// remain absent when unknown; known zero values are preserved through pointers.
type DispatchOutput struct {
	ID               string           `json:"id"`
	AgentType        string           `json:"agent_type"`
	Status           string           `json:"status"`
	ProjectDir       string           `json:"project_dir"`
	PromptFile       *string          `json:"prompt_file,omitempty"`
	OutputFile       *string          `json:"output_file,omitempty"`
	PID              *int             `json:"pid,omitempty"`
	ExitCode         *int             `json:"exit_code,omitempty"`
	Name             *string          `json:"name,omitempty"`
	Model            *string          `json:"model,omitempty"`
	Turns            int              `json:"turns"`
	Commands         int              `json:"commands"`
	Messages         int              `json:"messages"`
	InputTokens      int              `json:"in_tokens"`
	OutputTokens     int              `json:"out_tokens"`
	CacheHits        *int             `json:"cache_hits,omitempty"`
	CreatedAt        int64            `json:"created_at"`
	StartedAt        *int64           `json:"started_at,omitempty"`
	CompletedAt      *int64           `json:"completed_at,omitempty"`
	VerdictStatus    *string          `json:"verdict_status,omitempty"`
	VerdictSummary   *string          `json:"verdict_summary,omitempty"`
	ErrorMessage     *string          `json:"error_message,omitempty"`
	FailureClass     *string          `json:"failure_class,omitempty"`
	SandboxSpec      *json.RawMessage `json:"sandbox_spec,omitempty"`
	SandboxEffective *json.RawMessage `json:"sandbox_effective,omitempty"`
	ScopeID          *string          `json:"scope_id,omitempty"`
	ParentID         *string          `json:"parent_id,omitempty"`
}

// ToOutput converts a stored dispatch to its CLI representation.
func ToOutput(d *Dispatch) DispatchOutput {
	out := DispatchOutput{
		ID: d.ID, AgentType: d.AgentType, Status: d.Status, ProjectDir: d.ProjectDir,
		PromptFile: d.PromptFile, OutputFile: d.OutputFile, PID: d.PID, ExitCode: d.ExitCode,
		Name: d.Name, Model: d.Model, Turns: d.Turns, Commands: d.Commands, Messages: d.Messages,
		InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CacheHits: d.CacheHits,
		CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, CompletedAt: d.CompletedAt,
		VerdictStatus: d.VerdictStatus, VerdictSummary: d.VerdictSummary, ErrorMessage: d.ErrorMessage,
		ScopeID: d.ScopeID, ParentID: d.ParentID,
		FailureClass: d.QuarantineReason,
	}
	if d.SandboxSpec != nil {
		value := json.RawMessage(*d.SandboxSpec)
		out.SandboxSpec = &value
	}
	if d.SandboxEffective != nil {
		value := json.RawMessage(*d.SandboxEffective)
		out.SandboxEffective = &value
	}
	return out
}
