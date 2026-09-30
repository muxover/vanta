package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	Requests.Add(3)
	defer Requests.Add(-3)

	w := httptest.NewRecorder()
	Handler(w, nil)
	body := w.Body.String()
	if !strings.Contains(body, "\nvanta_requests_total 3\n") {
		t.Fatalf("missing counter:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE vanta_connections_active gauge\n") {
		t.Fatalf("missing type line:\n%s", body)
	}
	if n := strings.Count(body, "# TYPE "); n != len(all) {
		t.Fatalf("%d metrics, want %d", n, len(all))
	}
}
