package proxy

import (
	"encoding/json"
	"testing"
)

// ── InjectOwner ───────────────────────────────────────────────────────────────

func TestInjectOwner_EmptyBody(t *testing.T) {
	wallet := "0xABCD"
	out, err := InjectOwner(nil, wallet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	labels, ok := m["labels"].(map[string]any)
	if !ok {
		t.Fatal("labels field missing or wrong type")
	}
	if labels[ownerLabel] != wallet {
		t.Errorf("daytona-owner: got %v want %v", labels[ownerLabel], wallet)
	}
	if m["autostopInterval"] != float64(0) {
		t.Errorf("autostopInterval: got %v want 0", m["autostopInterval"])
	}
	if m["autoarchiveInterval"] != float64(0) {
		t.Errorf("autoarchiveInterval: got %v want 0", m["autoarchiveInterval"])
	}
}

func TestInjectOwner_OverwritesExistingOwner(t *testing.T) {
	wallet := "0xLEGIT"
	body := []byte(`{"labels":{"daytona-owner":"0xATTACKER","other":"val"}}`)

	out, err := InjectOwner(body, wallet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck
	labels := m["labels"].(map[string]any)

	if labels[ownerLabel] != wallet {
		t.Errorf("daytona-owner should be overwritten: got %v", labels[ownerLabel])
	}
	if labels["other"] != "val" {
		t.Error("other labels should be preserved")
	}
}

func TestInjectOwner_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"name":"my-sandbox","image":"ubuntu"}`)
	out, err := InjectOwner(body, "0xWALLET")
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	if m["name"] != "my-sandbox" {
		t.Errorf("name field lost: %v", m["name"])
	}
	if m["image"] != "ubuntu" {
		t.Errorf("image field lost: %v", m["image"])
	}
}

func TestInjectOwner_ForcesAutostopToZero(t *testing.T) {
	// User tries to set autostop; proxy must override to 0
	body := []byte(`{"autostopInterval":3600,"autoarchiveInterval":7200}`)
	out, err := InjectOwner(body, "0xW")
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	if m["autostopInterval"] != float64(0) {
		t.Errorf("autostopInterval should be 0, got %v", m["autostopInterval"])
	}
	if m["autoarchiveInterval"] != float64(0) {
		t.Errorf("autoarchiveInterval should be 0, got %v", m["autoarchiveInterval"])
	}
}

func TestInjectOwner_InvalidJSON(t *testing.T) {
	_, err := InjectOwner([]byte(`not json`), "0xW")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ── StripOwnerLabel ───────────────────────────────────────────────────────────

func TestStripOwnerLabel_RemovesKey(t *testing.T) {
	body := []byte(`{"daytona-owner":"0xHACKER","env":"prod"}`)
	out, err := StripOwnerLabel(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck

	if _, exists := m[ownerLabel]; exists {
		t.Error("daytona-owner should have been stripped")
	}
	if m["env"] != "prod" {
		t.Error("other keys should be preserved")
	}
}

func TestStripOwnerLabel_KeyAbsent(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	out, err := StripOwnerLabel(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(out, &m) //nolint:errcheck
	if m["foo"] != "bar" {
		t.Error("existing keys should be preserved when daytona-owner is absent")
	}
}

func TestStripOwnerLabel_InvalidJSON(t *testing.T) {
	_, err := StripOwnerLabel([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestStripOwnerLabel_EmptyObject(t *testing.T) {
	out, err := StripOwnerLabel([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "{}" {
		t.Errorf("unexpected output: %s", out)
	}
}
