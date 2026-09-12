package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeRecord reads at most 1 MiB and enforces one closed JSON object,
// recursively unique keys, and the exact ObservationInput field set.
func DecodeRecord(r io.Reader) (ObservationInput, []byte, error) {
	var input ObservationInput
	data, err := io.ReadAll(io.LimitReader(r, MaxRecordBytes+1))
	if err != nil {
		return input, nil, fmt.Errorf("read usage record: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return input, nil, fmt.Errorf("usage record exceeds %d bytes", MaxRecordBytes)
	}
	if err := rejectDuplicateKeysAndTrailing(data); err != nil {
		return input, nil, err
	}
	if err := requireInputKeys(data); err != nil {
		return input, nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return input, nil, fmt.Errorf("decode usage record: %w", err)
	}
	if err := input.Validate(); err != nil {
		return input, nil, fmt.Errorf("validate usage record: %w", err)
	}
	canonical, err := CanonicalJSON(input)
	if err != nil {
		return input, nil, fmt.Errorf("canonicalize usage record: %w", err)
	}
	return input, canonical, nil
}

func requireInputKeys(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode usage record: %w", err)
	}
	if err := requireKeys("usage record", root, []string{
		"id", "provider", "source", "kind", "status", "reason", "captured_at", "source_at",
		"interval_start", "interval_end", "quota_bucket", "quota_reset_at", "counters", "identity",
		"execution_refs", "payload_sha256", "supersedes",
	}); err != nil {
		return err
	}
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(root["identity"], &identity); err != nil {
		return errors.New("identity must be an object")
	}
	if err := requireKeys("identity", identity, []string{"native_thread_id", "native_session_id", "account"}); err != nil {
		return err
	}
	var account map[string]json.RawMessage
	if err := json.Unmarshal(identity["account"], &account); err != nil {
		return errors.New("identity.account must be an object")
	}
	if err := requireKeys("identity.account", account, []string{"coverage", "safe_ref", "reasons"}); err != nil {
		return err
	}
	if isJSONNull(account["reasons"]) {
		return errors.New("identity.account.reasons must be an array")
	}
	var refs map[string]json.RawMessage
	if err := json.Unmarshal(root["execution_refs"], &refs); err != nil {
		return errors.New("execution_refs must be an object")
	}
	if err := requireKeys("execution_refs", refs, []string{
		"dispatch_id", "session_row_id", "run_id", "task_id", "dispatch_request_id",
		"enrollment_id", "execution_id", "attempt_id",
	}); err != nil {
		return err
	}
	if isJSONNull(root["counters"]) {
		return errors.New("counters must be an array of objects")
	}
	var counters []map[string]json.RawMessage
	if err := json.Unmarshal(root["counters"], &counters); err != nil {
		return errors.New("counters must be an array of objects")
	}
	for i, counter := range counters {
		if err := requireKeys(fmt.Sprintf("counters[%d]", i), counter, []string{"name", "value", "unit", "semantics", "subset_of"}); err != nil {
			return err
		}
		// json.Number also accepts quoted numeric strings; the wire contract does not.
		if raw := bytes.TrimSpace(counter["value"]); !isJSONNull(raw) && !jsonNumberPattern.Match(raw) {
			return fmt.Errorf("counters[%d].value must be a nonnegative JSON number or null", i)
		}
	}
	return nil
}

func isJSONNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func requireKeys(path string, object map[string]json.RawMessage, keys []string) error {
	if object == nil {
		return fmt.Errorf("%s must be an object", path)
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("%s is missing required field %q", path, key)
		}
	}
	return nil
}

func rejectDuplicateKeysAndTrailing(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode usage record: %w", err)
	}
	delim, ok := first.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("usage record must be one JSON object")
	}
	if err := scanContainer(dec, delim); err != nil {
		return err
	}
	if token, err := dec.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("decode usage record: %w", err)
		}
		return fmt.Errorf("usage record has trailing JSON value %v", token)
	}
	return nil
}

func scanValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode usage record: %w", err)
	}
	if delim, ok := token.(json.Delim); ok {
		if delim != '{' && delim != '[' {
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
		return scanContainer(dec, delim)
	}
	return nil
}

func scanContainer(dec *json.Decoder, delim json.Delim) error {
	if delim == '{' {
		seen := make(map[string]struct{})
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return fmt.Errorf("decode usage record: %w", err)
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("usage record object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanValue(dec); err != nil {
				return err
			}
		}
	} else {
		for dec.More() {
			if err := scanValue(dec); err != nil {
				return err
			}
		}
	}
	end, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode usage record: %w", err)
	}
	want := json.Delim('}')
	if delim == '[' {
		want = ']'
	}
	if end != want {
		return fmt.Errorf("unexpected JSON delimiter %q", end)
	}
	return nil
}
