package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
)

func TestPrintFinding(t *testing.T) {
	initFailed := ops.Finding{
		Severity:  ops.SeverityError,
		Objects:   []string{"pod/squad-db-backup-29918057-zwwn7"},
		Container: "preflight",
		Reason:    "Failed",
		Message:   "as an init container, it exited with code 3 (Error)",
		Next:      "read its logs: `tug logs pod/squad-db-backup-29918057-zwwn7 -c preflight -n squad`",
	}
	jobFailed := ops.Finding{
		Severity: ops.SeverityError,
		Objects:  []string{"job/migrate"},
		Reason:   "BackoffLimitExceeded",
		Message:  "job has reached the specified backoff limit",
		Next:     "read the failed pods' logs: `tug logs job/migrate`; a failed job does not retry, so recreate it once fixed",
	}
	on := render.Colors{Enabled: true}
	tests := []struct {
		name   string
		colors render.Colors
		width  int
		f      ops.Finding
		want   string
	}{
		{
			name: "command on a line of its own, without backticks",
			f:    initFailed,
			want: "error    pod/squad-db-backup-29918057-zwwn7, container preflight: Failed\n" +
				"         as an init container, it exited with code 3 (Error)\n" +
				"         next: read its logs:\n" +
				"               tug logs pod/squad-db-backup-29918057-zwwn7 -c preflight -n squad\n",
		},
		{
			name:  "text wraps to the terminal under its indent; the command does not",
			width: 52,
			f:     initFailed,
			want: "error    pod/squad-db-backup-29918057-zwwn7, container preflight: Failed\n" +
				"         as an init container, it exited with code 3\n" +
				"         (Error)\n" +
				"         next: read its logs:\n" +
				"               tug logs pod/squad-db-backup-29918057-zwwn7 -c preflight -n squad\n",
		},
		{
			name: "a command inside a sentence keeps its backticks without colour",
			f:    jobFailed,
			want: "error    job/migrate: BackoffLimitExceeded\n" +
				"         job has reached the specified backoff limit\n" +
				"         next: read the failed pods' logs: `tug logs job/migrate`; a failed job does not retry, so recreate it once fixed\n",
		},
		{
			name:   "with colour, commands are cyan and lose their backticks",
			colors: on,
			f:      jobFailed,
			want: on.Red("error   ") + " job/migrate: " + on.Bold("BackoffLimitExceeded") + "\n" +
				"         job has reached the specified backoff limit\n" +
				"         " + on.Dim("next:") + " read the failed pods' logs: " + on.Cyan("tug logs job/migrate") +
				"; a failed job does not retry, so recreate it once fixed\n",
		},
		{
			name:   "a next step that is only a command",
			colors: on,
			f:      ops.Finding{Severity: ops.SeverityInfo, Objects: []string{"deploy/api"}, Reason: "Progressing", Message: "is rolling out", Next: "`tug rollout status deploy/api`"},
			want: on.Dim("info    ") + " deploy/api: " + on.Bold("Progressing") + "\n" +
				"         is rolling out\n" +
				"         " + on.Dim("next:") + " " + on.Cyan("tug rollout status deploy/api") + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			printFinding(&b, tt.colors, tt.width, tt.f)
			if got := b.String(); got != tt.want {
				t.Errorf("printFinding =\n%s\nwant\n%s", got, tt.want)
			}
			if !tt.colors.Enabled && tt.f.Next == initFailed.Next && strings.Contains(b.String(), "`") {
				t.Errorf("printFinding left backticks around a command on its own line:\n%s", b.String())
			}
		})
	}
}
