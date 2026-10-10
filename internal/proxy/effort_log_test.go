package proxy_test

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/router"
)

// effortBody is a main-loop turn whose client asks for the given effort
// ("" asks for none).
func effortBody(effort string) string {
	config := ""
	if effort != "" {
		config = `"output_config":{"effort":"` + effort + `"},`
	}
	return `{"model":"claude-opus-4-7","max_tokens":256,` + config +
		`"messages":[{"role":"user","content":"hello"}]}`
}

// The completion line names the effort the client asked for, what the served
// model received and what decided it.
func TestCompletionLog_ReportsRequestedAndSentEffort(t *testing.T) {
	cases := map[string]struct {
		body                  string
		override              string
		requested, sent, from string
	}{
		"client level passes through": {body: effortBody("medium"), requested: "medium", sent: "medium", from: "client"},
		"header overrides the client": {body: effortBody("medium"), override: "low", requested: "medium", sent: "low", from: "user"},
		"no effort anywhere":          {body: effortBody(""), requested: "", sent: "", from: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRuleFixture(t, "test-effort-log", opus5Decision, defaultTestMapping)
			var logs bytes.Buffer
			ctx := observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(&logs, nil)))
			if tc.override != "" {
				ctx = router.WithRoutingKnobs(ctx, &router.Overrides{ForceEffort: tc.override})
			}

			f.serve(t, ctx, tc.body, nil)

			line := completionLine(t, &logs)
			assert.Equal(t, tc.requested, line["effort_requested"])
			assert.Equal(t, tc.sent, line["effort_sent"])
			assert.Equal(t, tc.from, line["effort_source"])
		})
	}
}
