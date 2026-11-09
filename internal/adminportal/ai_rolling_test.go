package adminportal

import (
	"bytes"
	"github.com/tellyouwhat/backend/internal/airollout"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"testing"
	"time"
)

func TestRollingConfirmationBindsActorActionTargetAndExpiry(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }, config: Config{PreviewSigningKey: bytes.Repeat([]byte{7}, 32)}}
	in := airollout.Input{Endpoint: "ep-owned", Action: "start", Target: arkcontrol.FoundationModel{Name: "doubao-seed-2-0-lite", Version: "260428"}}
	claim := rollingClaim{Actor: "administrator", Input: in, Expires: now.Add(time.Minute).Unix()}
	token := s.signRolling(claim)
	if _, ok := s.readRollingToken(token, claim.Actor, in); !ok {
		t.Fatal("valid token rejected")
	}
	if _, ok := s.readRollingToken(token, "other", in); ok {
		t.Fatal("cross-actor confirmation")
	}
	changed := in
	changed.Action = "cancel"
	if _, ok := s.readRollingToken(token, claim.Actor, changed); ok {
		t.Fatal("cross-action confirmation")
	}
	changed = in
	changed.Target.Version = "260215"
	if _, ok := s.readRollingToken(token, claim.Actor, changed); ok {
		t.Fatal("changed target confirmation")
	}
	if _, ok := s.readRollingToken(token+"x", claim.Actor, in); ok {
		t.Fatal("tampered signature")
	}
	now = now.Add(time.Minute)
	if _, ok := s.readRollingToken(token, claim.Actor, in); ok {
		t.Fatal("expired confirmation")
	}
}
