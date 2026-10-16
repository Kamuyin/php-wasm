package runtimehost

import (
	"strings"
	"testing"
)

func TestExceptionHandlerExitKeepsFastCGIAlive(t *testing.T) {
	out, _ := runPHP(t, `
set_exception_handler(function($error) {
    http_response_code(404);
    echo 'HANDLED_ERROR';
    exit;
});
throw new RuntimeException('missing file');
`, []Config{{Version: 1}, {Version: 1}}, true)
	if strings.Count(out, "HANDLED_ERROR") != 2 || strings.Count(out, "Status: 404") != 2 {
		t.Fatalf("exception handler did not finish both requests: %s", out)
	}
}
