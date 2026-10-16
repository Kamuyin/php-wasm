package runtimehost

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Finding separates a heuristic input signal from what the interpreter did.
// Native structural SQL evidence and request-derived executable PHP syntax
// can be confirmed. A successful SQL call alone is never confirmed SQLi.
type Finding struct {
	Class          string `json:"class"`
	Stage          string `json:"stage"`
	Source         string `json:"source"`
	Sink           string `json:"sink,omitempty"`
	CallID         int    `json:"call_id,omitempty"`
	CompileID      int    `json:"compile_id,omitempty"`
	Evidence       string `json:"evidence"`
	ExploitVerdict string `json:"exploit_verdict"`
}

type inputValue struct{ source, value string }

// Analyzer is request-local and not concurrent. Use one instance per PHP module
// and consume events in order. request.start/end reset all correlation state.
// These rules are examples with explicit evidence strength, not a comprehensive
// exploit detector or a dynamic taint tracker.
type Analyzer struct {
	inputs   []inputValue
	calls    map[int][]Finding
	compiles map[int][]Finding
}

var sqlPattern = regexp.MustCompile(`(?i)(\bunion\s+(all\s+)?select\b|['"]\s*(or|and)\s+|;\s*(select|drop|insert|update|delete)\b|\b(sleep|benchmark)\s*\()`)
var commandPattern = regexp.MustCompile("(?i)(\\$\\(|`[^`]+`|;\\s*(id|whoami|cat|curl|wget|sh|bash)\\b)")

func inputClass(value string) []string {
	var classes []string
	if sqlPattern.MatchString(value) {
		classes = append(classes, "sql_injection")
	}
	lower := strings.ToLower(value)
	if strings.Contains(value, "../") || strings.Contains(value, `..\`) || strings.Contains(lower, "php://") || strings.Contains(lower, "phar://") {
		classes = append(classes, "file_inclusion_or_traversal")
	}
	if commandPattern.MatchString(value) {
		classes = append(classes, "command_injection")
	}
	return classes
}

func (a *Analyzer) Consume(raw json.RawMessage) ([]Finding, error) {
	var event map[string]any
	if len(raw) > MaxEventBytes {
		return nil, fmt.Errorf("runtime event exceeds limit")
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	if event["version"] != float64(1) {
		return nil, fmt.Errorf("unsupported event version")
	}
	id := func(key string) int { n, _ := event[key].(float64); return int(n) }
	text := func(key string) string { s, _ := event[key].(string); return s }
	var findings []Finding
	switch text("kind") {
	case "request.start":
		a.inputs = nil
		a.calls = map[int][]Finding{}
		a.compiles = map[int][]Finding{}
		var walk func(string, any, int)
		walk = func(path string, value any, depth int) {
			if depth > 4 || len(a.inputs) >= 64 {
				return
			}
			if s, ok := value.(string); ok {
				if len(s) >= 4 && len(s) <= 16384 {
					a.inputs = append(a.inputs, inputValue{path, s})
				}
				return
			}
			if object, ok := value.(map[string]any); ok && object["type"] == "array" {
				switch items := object["items"].(type) {
				case map[string]any:
					for key, item := range items {
						walk(path+"."+key, item, depth+1)
					}
				case []any:
					for i, item := range items {
						walk(fmt.Sprintf("%s.%d", path, i), item, depth+1)
					}
				}
			}
		}
		walk("get", event["get"], 0)
		walk("post", event["post"], 0)
		for _, input := range a.inputs {
			for _, class := range inputClass(input.value) {
				findings = append(findings, Finding{Class: class, Stage: "input_pattern", Source: input.source, Evidence: "heuristic_pattern", ExploitVerdict: "unconfirmed"})
			}
		}
	case "security.evidence":
		if text("class") == "sql_injection" || text("class") == "database_compatibility" {
			var finding Finding
			if err := json.Unmarshal(raw, &finding); err != nil {
				return nil, err
			}
			findings = append(findings, finding)
		}
	case "call.begin":
		if event["operator_hook"] == true {
			break
		}
		arguments, _ := event["arguments"].([]any)
		for _, input := range a.inputs {
			matched := false
			for _, argument := range arguments {
				if s, ok := argument.(string); ok && strings.Contains(s, input.value) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			for _, class := range inputClass(input.value) {
				if (class == "sql_injection" && text("sink") == "database") || (class == "command_injection" && text("sink") == "process") || (class == "file_inclusion_or_traversal" && text("sink") == "file") {
					finding := Finding{Class: class, Stage: "sink_reached", Source: input.source, Sink: text("function"), CallID: id("call_id"), Evidence: "input_bytes_present_in_argument_not_taint_proof", ExploitVerdict: "unconfirmed"}
					findings = append(findings, finding)
					if a.calls != nil {
						a.calls[id("call_id")] = append(a.calls[id("call_id")], finding)
					}
				}
			}
		}
	case "call.end":
		for _, finding := range a.calls[id("call_id")] {
			finding.Stage = "operation_returned"
			finding.Evidence = "runtime_return_observed_not_exploit_proof"
			if text("completion") != "returned" || event["result"] == false {
				finding.Stage = "operation_failed"
			}
			if text("operation_outcome") == "unavailable" {
				finding.Stage = "operation_unavailable"
			}
			if strings.Contains(finding.Sink, "prepare") && finding.Stage == "operation_returned" {
				finding.Stage = "statement_prepared"
			}
			findings = append(findings, finding)
		}
		delete(a.calls, id("call_id"))
	case "code.compile":
		if text("code_type") == "eval" && executableOrigin(text("source"), event["provenance"]) {
			finding := Finding{Class: "input_code_execution", Stage: "compile_attempt", Sink: text("target"), CompileID: id("compile_id"), Evidence: "native_request_derived_executable_syntax", ExploitVerdict: "unconfirmed"}
			findings = append(findings, finding)
			if a.compiles != nil {
				a.compiles[id("compile_id")] = []Finding{finding}
			}
			break
		}
		for _, input := range a.inputs {
			class, evidence := "", ""
			if text("code_type") == "eval" && text("source") == input.value {
				class = "input_code_execution"
				evidence = "exact_input_equals_compiler_source"
			}
			if text("code_type") == "file" && strings.Contains(text("target"), input.value) {
				for _, candidate := range inputClass(input.value) {
					if candidate == "file_inclusion_or_traversal" {
						class = candidate
						evidence = "input_bytes_present_in_include_path_not_taint_proof"
					}
				}
			}
			if class != "" {
				finding := Finding{Class: class, Stage: "compile_attempt", Source: input.source, Sink: text("target"), CompileID: id("compile_id"), Evidence: evidence, ExploitVerdict: "unconfirmed"}
				findings = append(findings, finding)
				if a.compiles != nil {
					a.compiles[id("compile_id")] = append(a.compiles[id("compile_id")], finding)
				}
			}
		}
	case "code.compiled", "code.enter", "code.leave":
		for index, finding := range a.compiles[id("compile_id")] {
			switch text("kind") {
			case "code.compiled":
				if event["compiled"] == true {
					finding.Stage = "compiled"
				} else {
					finding.Stage = "compile_failed"
				}
			case "code.enter":
				finding.Stage = "execution_entered"
				if finding.Evidence == "native_request_derived_executable_syntax" {
					finding.ExploitVerdict = "confirmed"
					a.compiles[id("compile_id")][index] = finding
				}
			case "code.leave":
				finding.Stage = "execution_returned"
				if text("completion") != "returned" {
					finding.Stage = "execution_interrupted"
				}
			}
			findings = append(findings, finding)
		}
		if text("kind") == "code.leave" || event["compiled"] == false {
			delete(a.compiles, id("compile_id"))
		}
	case "request.end":
		a.inputs = nil
		a.calls = nil
		a.compiles = nil
	}
	return findings, nil
}

// Conservative PHP lexical check: request bytes inside quoted literals or
// comments cannot establish code injection. Heredoc is left unconfirmed.
func executableOrigin(source string, provenance any) bool {
	if strings.Contains(source, "<<<") {
		return false
	}
	ranges, _ := provenance.([]any)
	requestByte := func(index int) bool {
		for _, raw := range ranges {
			r, ok := raw.(map[string]any)
			if !ok || r["origin"] != "request" {
				continue
			}
			start, _ := r["start"].(float64)
			length, _ := r["length"].(float64)
			if float64(index) >= start && float64(index) < start+length {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(source); {
		if strings.ContainsRune(" \t\r\n", rune(source[i])) {
			i++
			continue
		}
		if strings.HasPrefix(source[i:], "//") || source[i] == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(source[i:], "/*") {
			end := strings.Index(source[i+2:], "*/")
			if end < 0 {
				return false
			}
			i += end + 4
			continue
		}
		if source[i] == '\'' || source[i] == '"' || source[i] == '`' {
			quote := source[i]
			i++
			for i < len(source) {
				if source[i] == '\\' {
					i += 2
					continue
				}
				if source[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}
		if requestByte(i) {
			return true
		}
		i++
	}
	return false
}
