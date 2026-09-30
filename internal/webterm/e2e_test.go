package webterm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestE2E drives a running web terminal over its WebSocket protocol. It is
// skipped unless MSNW_WEBTERM_URL (e.g. ws://127.0.0.1:8081/ws) and
// MSNW_WEBTERM_TARGET (user@name[:port]) are set; MSNW_WEBTERM_PASSWORD is
// optional. Used by the manual/CI end-to-end scripts, not by "go test" alone.
func TestE2E(t *testing.T) {
	url, target := os.Getenv("MSNW_WEBTERM_URL"), os.Getenv("MSNW_WEBTERM_TARGET")
	if url == "" || target == "" {
		t.Skip("MSNW_WEBTERM_URL / MSNW_WEBTERM_TARGET not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	h, _ := json.Marshal(hello{Target: target, Password: os.Getenv("MSNW_WEBTERM_PASSWORD"), Cols: 100, Rows: 30})
	if err := ws.Write(ctx, websocket.MessageText, h); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var statuses []string
	deadline := time.After(30 * time.Second)
	sent := false
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout; statuses=%v output=%q", statuses, out.String())
		default:
		}
		rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
		typ, msg, err := ws.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("read: %v (statuses=%v output=%q)", err, statuses, out.String())
		}
		if typ == websocket.MessageText {
			statuses = append(statuses, strings.TrimPrefix(string(msg), "\x00"))
			if strings.HasPrefix(string(msg), "\x00connected") && !sent {
				sent = true
				// resize, then run a command that proves we are in a real pty
				ws.Write(ctx, websocket.MessageText, []byte(`{"resize":[120,40]}`))
				ws.Write(ctx, websocket.MessageBinary, []byte("echo WEBTERM_$(tput cols)x$(tput lines)_OK; exit\n"))
			}
			if strings.HasPrefix(string(msg), "\x00session closed") {
				if !strings.Contains(out.String(), "WEBTERM_120x40_OK") {
					t.Fatalf("expected the command output, got %q (statuses=%v)", out.String(), statuses)
				}
				t.Logf("statuses: %v", statuses)
				return
			}
			if strings.Contains(string(msg), "failed") || strings.HasPrefix(string(msg), "\x00ssh:") {
				t.Fatalf("error status: %s", msg)
			}
			continue
		}
		out.Write(msg)
	}
}
