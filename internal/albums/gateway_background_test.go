package albums

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/lifecycle"
)

func TestGatewayBatchWaitsForActivationAndPreventsSealingUntilSettled(t *testing.T) {
	control, err := lifecycle.New("blue")
	if err != nil {
		t.Fatal(err)
	}
	startup, startupCancel := context.WithCancel(lifecycle.WithController(context.Background(), control))
	ctx, cancel := context.WithCancel(context.WithoutCancel(startup))
	defer cancel()
	startupCancel()
	command := func(action string) int {
		r := httptest.NewRequest("POST", "/control/"+action, strings.NewReader(`{"bootID":"`+control.Status().BootID+`"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		control.Control().ServeHTTP(w, r)
		return w.Code
	}
	entered, settled := make(chan struct{}), make(chan struct{})
	completed := make(chan bool, 1)
	defer func() {
		select {
		case <-settled:
		default:
			close(settled)
		}
	}()
	go func() {
		completed <- runGatewayBatch(ctx, func() { close(entered); <-settled })
	}()
	select {
	case <-entered:
		t.Fatal("standby acquired work")
	case <-completed:
		t.Fatal("startup cancellation killed the long-lived worker")
	case <-time.After(30 * time.Millisecond):
	}
	if command("serve") != 200 || command("resume") != 200 {
		t.Fatal("activation failed")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("active worker did not acquire work")
	}
	if control.Status().Background != 1 || command("pause") != 200 || command("seal") != 409 || ctx.Err() != nil {
		t.Fatal("pause lost the in-flight acquisition/settlement interval")
	}
	close(settled)
	select {
	case admitted := <-completed:
		if !admitted {
			t.Fatal("admitted batch not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("settled batch did not exit")
	}
	if control.Status().Background != 0 || command("seal") != 200 {
		t.Fatal("settlement did not release the drain counter")
	}
	go func() { completed <- runGatewayBatch(ctx, func() { t.Error("paused worker acquired work") }) }()
	cancel()
	select {
	case admitted := <-completed:
		if admitted {
			t.Fatal("canceled batch admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not stop the paused worker")
	}
}
