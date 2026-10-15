package albums

import (
	"context"
	"errors"
	"log/slog"
)

var ErrLogin = errors.New("album login failed; start a new Apple authorization")

type AppleGrantExchanger interface {
	Exchange(context.Context, string, string, string) (AppleGrant, error)
}
type LoginAccounts interface {
	RegisterAppleLogin(context.Context, AppleGrant) (AccountLogin, error)
}
type LoginSessions interface {
	Issue(context.Context, AccountLogin) (AlbumSession, error)
}

// AppleLoginRequest contains client proof, never an account ID or Apple subject.
// HTTP integration must limit body size and reject unknown fields.
type AppleLoginRequest struct {
	ChallengeID       string `json:"challengeID"`
	Proof             string `json:"proof"`
	AuthorizationCode string `json:"authorizationCode"`
	IdentityToken     string `json:"identityToken"`
}

func (AppleLoginRequest) String() string   { return "[redacted album login request]" }
func (AppleLoginRequest) GoString() string { return "[redacted album login request]" }
func (AppleLoginRequest) LogValue() slog.Value {
	return slog.StringValue("[redacted album login request]")
}

type LoginService struct {
	challenges *LoginChallengeService
	exchanger  AppleGrantExchanger
	accounts   LoginAccounts
	sessions   LoginSessions
}

func NewLoginService(challenges *LoginChallengeService, exchanger AppleGrantExchanger, accounts LoginAccounts, sessions LoginSessions) (*LoginService, error) {
	if challenges == nil || exchanger == nil || accounts == nil || sessions == nil {
		return nil, errors.New("album login dependencies are required")
	}
	return &LoginService{challenges, exchanger, accounts, sessions}, nil
}
func (s *LoginService) Challenge(ctx context.Context) (LoginChallenge, error) {
	return s.challenges.Issue(ctx)
}

// Login never retries the one-use Apple code. If an account was committed but
// session issuance fails, the next fresh Apple login recovers that same account.
// HTTP callers receive a generic error, never provider/database credentials.
func (s *LoginService) Login(ctx context.Context, request AppleLoginRequest) (AlbumSession, error) {
	if request.AuthorizationCode == "" || len(request.AuthorizationCode) > 4096 || request.IdentityToken == "" || len(request.IdentityToken) > 64<<10 {
		return AlbumSession{}, ErrLogin
	}
	nonce, err := s.challenges.Consume(ctx, request.ChallengeID, request.Proof)
	if err != nil {
		return AlbumSession{}, ErrLogin
	}
	grant, err := s.exchanger.Exchange(ctx, request.AuthorizationCode, request.IdentityToken, nonce)
	if err != nil {
		return AlbumSession{}, ErrLogin
	}
	login, err := s.accounts.RegisterAppleLogin(ctx, grant)
	if err != nil {
		return AlbumSession{}, ErrLogin
	}
	session, err := s.sessions.Issue(ctx, login)
	if err != nil {
		return AlbumSession{}, ErrLogin
	}
	return session, nil
}
