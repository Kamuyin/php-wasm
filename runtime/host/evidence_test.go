package runtimehost

import (
	"encoding/json"
	"testing"
)

func analyzeEvent(t *testing.T, a *Analyzer, event map[string]any) []Finding {
	t.Helper()
	event["version"] = 1
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := a.Consume(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.ExploitVerdict != "unconfirmed" {
			t.Fatalf("unjustified exploit confirmation: %+v", finding)
		}
	}
	return findings
}

func TestEvidenceDoesNotPromoteSQLSuccessToConfirmedInjection(t *testing.T) {
	for _, test := range []struct {
		result any
		stage  string
	}{{false, "operation_failed"}, {float64(1), "operation_returned"}} {
		var a Analyzer
		input := `' OR 1=1 --`
		start := map[string]any{"kind": "request.start", "get": map[string]any{"type": "array", "items": map[string]any{"q": input}}}
		if findings := analyzeEvent(t, &a, start); len(findings) != 1 || findings[0].Stage != "input_pattern" {
			t.Fatal(findings)
		}
		findings := analyzeEvent(t, &a, map[string]any{"kind": "call.begin", "call_id": 2, "function": "pdo::query", "sink": "database", "arguments": []string{"SELECT * FROM records WHERE name = '" + input + "'"}})
		if len(findings) != 1 || findings[0].Stage != "sink_reached" {
			t.Fatal(findings)
		}
		findings = analyzeEvent(t, &a, map[string]any{"kind": "call.end", "call_id": 2, "completion": "returned", "result": test.result})
		if len(findings) != 1 || findings[0].Stage != test.stage {
			t.Fatal(findings)
		}
		analyzeEvent(t, &a, map[string]any{"kind": "request.end"})
		if len(a.inputs) != 0 || len(a.calls) != 0 {
			t.Fatal("request state leaked")
		}
	}
}

func TestEvidenceRequiresCodeExecution(t *testing.T) {
	var a Analyzer
	analyzeEvent(t, &a, map[string]any{"kind": "request.start", "post": map[string]any{"type": "array", "items": map[string]any{"code": "return 42;"}}})
	findings := analyzeEvent(t, &a, map[string]any{"kind": "code.compile", "compile_id": 3, "code_type": "eval", "source": "return 42;", "target": "eval fixture"})
	if len(findings) != 1 || findings[0].Stage != "compile_attempt" {
		t.Fatal(findings)
	}
	findings = analyzeEvent(t, &a, map[string]any{"kind": "code.compiled", "compile_id": 3, "compiled": true})
	if len(findings) != 1 || findings[0].Stage != "compiled" {
		t.Fatal(findings)
	}
	findings = analyzeEvent(t, &a, map[string]any{"kind": "code.enter", "compile_id": 3})
	if len(findings) != 1 || findings[0].Stage != "execution_entered" {
		t.Fatal(findings)
	}
	findings = analyzeEvent(t, &a, map[string]any{"kind": "code.leave", "compile_id": 3, "completion": "returned"})
	if len(findings) != 1 || findings[0].Stage != "execution_returned" {
		t.Fatal(findings)
	}
}

func TestOperatorHooksDoNotBecomeExploitEvidence(t *testing.T) {
	var a Analyzer
	analyzeEvent(t, &a, map[string]any{"kind": "request.start", "get": map[string]any{"type": "array", "items": map[string]any{"path": "../secret"}}})
	findings := analyzeEvent(t, &a, map[string]any{"kind": "call.begin", "call_id": 1, "function": "file_get_contents", "sink": "file", "arguments": []string{"../secret"}, "operator_hook": true})
	if len(findings) != 0 {
		t.Fatal(findings)
	}
}
