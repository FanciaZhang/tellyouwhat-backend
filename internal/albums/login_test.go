package albums

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type loginPipelineFixture struct {
	steps []string
	fail  string
	nonce string
}

func (f *loginPipelineFixture) Exchange(_ context.Context, code, identity, nonce string) (AppleGrant, error) {
	f.steps = append(f.steps, "exchange")
	if code != "code" || identity != "identity" || nonce != f.nonce {
		return AppleGrant{}, errors.New("wrong challenge binding")
	}
	if f.fail == "exchange" {
		return AppleGrant{}, errors.New("private provider error")
	}
	return AppleGrant{Identity: freshAppleIdentity("verified-subject"), RefreshToken: "private-refresh"}, nil
}
func (f *loginPipelineFixture) RegisterAppleLogin(_ context.Context, grant AppleGrant) (AccountLogin, error) {
	f.steps = append(f.steps, "account")
	if grant.Identity.Subject != "verified-subject" || grant.RefreshToken != "private-refresh" {
		return AccountLogin{}, errors.New("unverified identity")
	}
	if f.fail == "account" {
		return AccountLogin{}, errors.New("private database error")
	}
	return AccountLogin{AccountID: "server-account", GrantID: "server-grant"}, nil
}
func (f *loginPipelineFixture) Issue(_ context.Context, login AccountLogin) (AlbumSession, error) {
	f.steps = append(f.steps, "session")
	if login.AccountID != "server-account" || login.GrantID != "server-grant" {
		return AlbumSession{}, errors.New("wrong account")
	}
	if f.fail == "session" {
		return AlbumSession{}, errors.New("private session error")
	}
	return AlbumSession{Token: "private-session", AccountID: login.AccountID}, nil
}
func TestLoginPipelineOnlyIssuesSessionAfterVerifiedAccount(t *testing.T) {
	for _, stage := range []string{"", "exchange", "account", "session"} {
		t.Run("failure_"+stage, func(t *testing.T) {
			store := &challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}
			challenges, _ := NewLoginChallengeService(store, time.Now)
			fixture := &loginPipelineFixture{fail: stage}
			service, err := NewLoginService(challenges, fixture, fixture, fixture)
			if err != nil {
				t.Fatal(err)
			}
			challenge, err := service.Challenge(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fixture.nonce = challenge.Nonce
			request := AppleLoginRequest{challenge.ID, challenge.Proof, "code", "identity"}
			result, err := service.Login(context.Background(), request)
			expected := map[string]string{"": "exchange,account,session", "exchange": "exchange", "account": "exchange,account", "session": "exchange,account,session"}[stage]
			if strings.Join(fixture.steps, ",") != expected {
				t.Fatal("unexpected login ordering", fixture.steps)
			}
			if stage == "" {
				if err != nil || result.Token != "private-session" || result.AccountID != "server-account" {
					t.Fatal("valid login failed", err)
				}
			} else if err != ErrLogin || result != (AlbumSession{}) {
				t.Fatal("failure leaked session or internal error", err)
			}
			if _, err := service.Login(context.Background(), request); err != ErrLogin || strings.Join(fixture.steps, ",") != expected {
				t.Fatal("replayed code reached provider")
			}
			if strings.Contains(fmt.Sprintf("%v %#v", request, request), challenge.Proof) {
				t.Fatal("request proof leaked")
			}
		})
	}
}
func TestLoginWrongProofDoesNotReachProviderOrConsumeChallenge(t *testing.T) {
	store := &challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}
	challenges, _ := NewLoginChallengeService(store, time.Now)
	fixture := &loginPipelineFixture{}
	service, _ := NewLoginService(challenges, fixture, fixture, fixture)
	challenge, _ := service.Challenge(context.Background())
	fixture.nonce = challenge.Nonce
	request := AppleLoginRequest{challenge.ID, "invalid", "code", "identity"}
	if _, err := service.Login(context.Background(), request); err != ErrLogin || len(fixture.steps) != 0 {
		t.Fatal("invalid proof reached provider")
	}
	request.Proof = challenge.Proof
	if _, err := service.Login(context.Background(), request); err != nil {
		t.Fatal("wrong proof consumed valid challenge")
	}
	if _, err := NewLoginService(nil, fixture, fixture, fixture); err == nil {
		t.Fatal("missing challenge service accepted")
	}
}
