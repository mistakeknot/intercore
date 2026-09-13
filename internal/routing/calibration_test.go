package routing

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeCalibration(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing-calibration.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func calibrationConfig(mode string) *Config {
	cfg := testConfig()
	cfg.Calibration.Mode = mode
	return cfg
}

func TestCalibrationEnforceUsesNamespacePhaseAliasesThenGlobal(t *testing.T) {
	path := writeCalibration(t, `{
  "schema_version": 2,
  "agents": {
    "worker": {
      "recommended_model": "opus", "confidence": 0.9, "evidence_sessions": 4, "propagation_eligible": true,
      "phases": {
        "ship": {"recommended_model": "sonnet", "confidence": 0.9, "evidence_sessions": 4, "propagation_eligible": true},
        "shipping": {"recommended_model": "haiku", "confidence": 0.9, "evidence_sessions": 4, "propagation_eligible": true},
        "plan": {"recommended_model": "haiku", "confidence": 0.69, "evidence_sessions": 4, "propagation_eligible": true},
        "implementation": {"recommended_model": "sonnet", "confidence": 0.9, "evidence_sessions": 3.0, "propagation_eligible": true}
      }
    }
  }
}`)
	artifact := ReadCalibration(path)
	r := NewResolver(calibrationConfig("enforce"))

	got := r.ResolveModelDetailed(ResolveOpts{Phase: "shipping", Agent: "plugin:worker"}, artifact)
	if got.Model != "haiku" || got.Calibration.Disposition != "applied" || value(got.Calibration.SelectedScope) != "phase:shipping" {
		t.Fatalf("exact phase did not win: %#v", got)
	}

	got = r.ResolveModelDetailed(ResolveOpts{Phase: "build", Agent: "plugin:worker"}, artifact)
	if got.Model != "sonnet" || value(got.Calibration.SelectedScope) != "phase:implementation" {
		t.Fatalf("phase alias did not resolve: %#v", got)
	}

	got = r.ResolveModelDetailed(ResolveOpts{Phase: "plan", Agent: "plugin:worker"}, artifact)
	if got.Model != "opus" || value(got.Calibration.SelectedScope) != "agent" || value(got.Calibration.FallbackReason) != "phase_recommendation_unavailable" {
		t.Fatalf("eligible global fallback not used: %#v", got)
	}
}

func TestCalibrationModesOverridesAndFloors(t *testing.T) {
	path := writeCalibration(t, `{"schema_version":2,"agents":{"worker":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true},"fd-correctness":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true},"fd-safety":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true}}}`)
	artifact := ReadCalibration(path)

	for _, tc := range []struct {
		name, mode, env, agent, wantModel, wantDisposition, wantReason string
	}{
		{"off", "off", "", "worker", "sonnet", "static", "calibration_mode_off"},
		{"shadow", "shadow", "", "worker", "sonnet", "shadow", "shadow_mode"},
		{"invalid policy", "bogus", "", "worker", "sonnet", "static", "invalid_calibration_mode"},
		{"env enforce", "off", "enforce", "worker", "haiku", "applied", ""},
		{"invalid env", "enforce", "bogus", "worker", "sonnet", "static", "invalid_calibration_mode"},
		{"static override", "enforce", "", "interflux:review:fd-safety", "opus", "static", "static_agent_override"},
		{"floor final", "enforce", "", "fd-correctness", "sonnet", "applied", "safety_floor_applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("INTERSPECT_ROUTING_MODE", "")
				_ = os.Unsetenv("INTERSPECT_ROUTING_MODE")
			} else {
				t.Setenv("INTERSPECT_ROUTING_MODE", tc.env)
			}
			r := NewResolver(calibrationConfig(tc.mode))
			got := r.ResolveModelDetailed(ResolveOpts{Agent: tc.agent}, artifact)
			if got.Model != tc.wantModel || got.Calibration.Disposition != tc.wantDisposition || value(got.Calibration.FallbackReason) != tc.wantReason {
				t.Fatalf("got %#v", got)
			}
		})
	}
}

func TestCalibrationSchemaCompatibilityAndEligibility(t *testing.T) {
	tests := []struct {
		name, body, wantModel, wantDisposition, wantReason string
	}{
		{"schema 1 diagnostic only", `{"schema_version":1,"agents":{"worker":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3}}}`, "sonnet", "static", "schema_1_diagnostic_only"},
		{"schema 2 requires eligibility", `{"schema_version":2,"agents":{"worker":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3}}}`, "sonnet", "static", "no_eligible_recommendation"},
		{"schema 3 ignores skills", `{"schema_version":3,"skills":{"x":{"score":1}},"agents":{"worker":{"recommended_model":"haiku","confidence":0.7,"evidence_sessions":3.0,"propagation_eligible":true}}}`, "haiku", "applied", "phase_recommendation_unavailable"},
		{"unsupported phase model falls global", `{"schema_version":2,"agents":{"worker":{"recommended_model":"opus","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true,"phases":{"build":{"recommended_model":"gpt-6-astra","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true}}}}}`, "opus", "applied", "phase_recommendation_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			artifact := ReadCalibration(writeCalibration(t, tc.body))
			got := NewResolver(calibrationConfig("enforce")).ResolveModelDetailed(ResolveOpts{Agent: "worker", Phase: "build"}, artifact)
			if got.Model != tc.wantModel || got.Calibration.Disposition != tc.wantDisposition || value(got.Calibration.FallbackReason) != tc.wantReason {
				t.Fatalf("got %#v", got)
			}
		})
	}
}

func TestCalibrationRejectsMalformedEnvelopeWithoutChangingStaticRoute(t *testing.T) {
	tests := []struct {
		name, body, reason string
	}{
		{"duplicate", `{"schema_version":2,"schema_version":3,"agents":{}}`, "duplicate_key"},
		{"trailing document", `{"schema_version":2,"agents":{}} {}`, "trailing_json"},
		{"overflow", `{"schema_version":2,"unknown":1e9999,"agents":{}}`, "nonfinite_number"},
		{"confidence underflow", `{"schema_version":2,"agents":{"worker":{"confidence":1e-9999}}}`, "numeric_underflow"},
		{"unsupported schema", `{"schema_version":4,"agents":{}}`, "unsupported_schema"},
		{"schema string", `{"schema_version":"2","agents":{}}`, "schema_wrong_type"},
		{"schema precision", `{"schema_version":2.0000000000000001,"agents":{}}`, "schema_wrong_type"},
		{"agents scalar", `{"schema_version":2,"agents":false}`, "agents_wrong_type"},
		{"confidence string", `{"schema_version":2,"agents":{"worker":{"recommended_model":"haiku","confidence":"0.9","evidence_sessions":3,"propagation_eligible":true}}}`, "confidence_wrong_type"},
		{"evidence boolean", `{"schema_version":2,"agents":{"worker":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":true,"propagation_eligible":true}}}`, "evidence_sessions_wrong_type"},
		{"eligibility string", `{"schema_version":2,"agents":{"worker":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":"true"}}}`, "propagation_eligible_wrong_type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			artifact := ReadCalibration(writeCalibration(t, tc.body))
			got := NewResolver(calibrationConfig("enforce")).ResolveModelDetailed(ResolveOpts{Agent: "worker"}, artifact)
			if got.Model != "sonnet" || got.Calibration.Disposition != "static" || value(got.Calibration.FallbackReason) != "invalid_calibration_artifact" {
				t.Fatalf("malformed artifact changed routing: %#v", got)
			}
			if !hasExclusion(got.Calibration.Exclusions, tc.reason) {
				t.Fatalf("missing %q exclusion: %#v", tc.reason, got.Calibration.Exclusions)
			}
		})
	}
}

func TestCalibrationUnreadableIsExplicitStatic(t *testing.T) {
	artifact := ReadCalibration(filepath.Join(t.TempDir(), "missing.json"))
	got := NewResolver(calibrationConfig("enforce")).ResolveModelDetailed(ResolveOpts{Agent: "worker"}, artifact)
	if got.Model != "sonnet" || value(got.Calibration.FallbackReason) != "calibration_artifact_unreadable" || got.Calibration.SHA256 != nil {
		t.Fatalf("unreadable artifact metadata = %#v", got)
	}
}

func TestCalibrationRequiresNonemptyAgentScope(t *testing.T) {
	artifact := ReadCalibration(writeCalibration(t, `{"schema_version":2,"agents":{"":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true}}}`))
	for _, agent := range []string{"", "plugin:"} {
		got := NewResolver(calibrationConfig("enforce")).ResolveModelDetailed(ResolveOpts{Agent: agent}, artifact)
		if got.Model != "sonnet" || value(got.Calibration.FallbackReason) != "agent_missing" {
			t.Fatalf("unscoped artifact affected %q: %#v", agent, got)
		}
	}
}

func TestCalibrationRevalidatesDefaultedMode(t *testing.T) {
	cfg := calibrationConfig("unsupported")
	cfg.Calibration.defaulted = true
	_, _, valid := NewResolver(cfg).calibrationMode()
	if valid {
		t.Fatal("defaulted mode accepted a subsequently invalid value")
	}
}

func TestCalibrationEligibilityBoundariesAreStaticAndExplicit(t *testing.T) {
	tests := []struct {
		name, fields, reason string
	}{
		{"confidence low", `"recommended_model":"haiku","confidence":0.699,"evidence_sessions":3,"propagation_eligible":true`, "confidence_below_threshold"},
		{"confidence precision", `"recommended_model":"haiku","confidence":0.69999999999999999,"evidence_sessions":3,"propagation_eligible":true`, "confidence_below_threshold"},
		{"session precision", `"recommended_model":"haiku","confidence":0.7,"evidence_sessions":2.9999999999999999,"propagation_eligible":true`, "evidence_sessions_not_integral"},
		{"confidence high", `"recommended_model":"haiku","confidence":1.001,"evidence_sessions":3,"propagation_eligible":true`, "confidence_above_maximum"},
		{"too few sessions", `"recommended_model":"haiku","confidence":0.7,"evidence_sessions":2,"propagation_eligible":true`, "insufficient_evidence_sessions"},
		{"fractional sessions", `"recommended_model":"haiku","confidence":0.7,"evidence_sessions":3.5,"propagation_eligible":true`, "evidence_sessions_not_integral"},
		{"false eligibility", `"recommended_model":"haiku","confidence":0.7,"evidence_sessions":3,"propagation_eligible":false`, "propagation_ineligible"},
		{"unsupported model", `"recommended_model":"fable","confidence":0.7,"evidence_sessions":3,"propagation_eligible":true`, "unsupported_model"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"schema_version":2,"agents":{"worker":{` + tc.fields + `}}}`
			artifact := ReadCalibration(writeCalibration(t, body))
			got := NewResolver(calibrationConfig("enforce")).ResolveModelDetailed(ResolveOpts{Agent: "worker"}, artifact)
			if got.Model != "sonnet" || value(got.Calibration.FallbackReason) != "no_eligible_recommendation" || !hasExclusion(got.Calibration.Exclusions, tc.reason) {
				t.Fatalf("ineligible value was not explicit: %#v", got)
			}
		})
	}
}

func TestCalibrationHashUsesExactBytes(t *testing.T) {
	body := "{\"schema_version\":3,\"agents\":{}}\n"
	artifact := ReadCalibration(writeCalibration(t, body))
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	if artifact.metadata.SHA256 == nil || *artifact.metadata.SHA256 != want {
		t.Fatalf("sha256 = %v, want %s", artifact.metadata.SHA256, want)
	}
}

func TestCalibrationDoesNotChangeFableWindowFallback(t *testing.T) {
	cfg := calibrationConfig("enforce")
	cfg.Subagents.Defaults.Model = "fable"
	artifact := ReadCalibration(writeCalibration(t, `{"schema_version":2,"agents":{}}`))
	r := NewResolver(cfg)

	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "")
	if got := r.ResolveModelDetailed(ResolveOpts{Agent: "worker"}, artifact).Model; got != "opus" {
		t.Fatalf("closed window = %q, want opus", got)
	}
	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "1")
	if got := r.ResolveModelDetailed(ResolveOpts{Agent: "worker"}, artifact).Model; got != "fable" {
		t.Fatalf("open window = %q, want fable", got)
	}
}

func TestRoleCalibrationReportsInvalidModeWithoutSelectingArm(t *testing.T) {
	artifact := ReadCalibration(writeCalibration(t, `{"schema_version":2,"agents":{}}`))
	metadata := NewResolver(calibrationConfig("invalid")).DiagnoseRoleCalibration(artifact)
	if metadata.Disposition != "static" || metadata.SelectedArm != nil || value(metadata.FallbackReason) != "invalid_calibration_mode" || !hasExclusion(metadata.Exclusions, "unsupported_artifact_kind") {
		t.Fatalf("role diagnostics = %#v", metadata)
	}
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func hasExclusion(exclusions []CalibrationExclusion, reason string) bool {
	for _, exclusion := range exclusions {
		if exclusion.Reason == reason {
			return true
		}
	}
	return false
}
