package runtimehost

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	for _, input := range []string{
		`{"version":2}`, `{"version":1,"unknown":true}`, `{"version":1} {}`,
		`{"version":1,"max_events":513}`, `{"version":1,"observe":["PDO::query"]}`,
		`{"version":1,"hooks":{"f":{"unexpected":"code"}}}`,
		`{"version":1,"bootstrap":"a\u0000b"}`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := DecodeConfig(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	if _, err := DecodeConfig(strings.NewReader(`{"version":1,"hooks":{"pdo::query":{"after":"return null;"}}}`)); err != nil {
		t.Fatal(err)
	}
}
