package albums

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAlbumAppleAssertionsCannotCrossAppIdentities(t *testing.T) {
	key := testAppleKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, audience := range []string{albumAppleAudience, albumDevelopmentAudience} {
		t.Run(audience, func(t *testing.T) {
			verifier := NewAppleIdentityVerifier(audience, appleTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != appleIssuer+"/auth/keys" {
					t.Fatal("unexpected Apple endpoint")
				}
				return appleResponse(testAppleJWKS(key, "key")), nil
			}))
			verifier.now = func() time.Time { return now }
			for _, signedAudience := range []string{albumAppleAudience, albumDevelopmentAudience} {
				claims := appleClaims(now)
				claims["aud"] = signedAudience
				identity, err := verifier.Verify(context.Background(), appleSigned(t, key, "key", claims), strings.Repeat("a", 64))
				if signedAudience == audience {
					if err != nil || identity.Audience != audience {
						t.Fatal("matching identity rejected", err)
					}
				} else if !errors.Is(err, ErrAppleIdentity) {
					t.Fatal("cross-app login accepted", err)
				}
				eventClaims := accountEventClaims(now)
				eventClaims["aud"] = signedAudience
				event, err := verifier.verifyAccountEvent(context.Background(), appleSigned(t, key, "key", eventClaims))
				if signedAudience == audience {
					if err != nil || event.Audience != audience {
						t.Fatal("matching account notification rejected", err)
					}
				} else if !errors.Is(err, errAppleNotification) {
					t.Fatal("cross-app account notification accepted", err)
				}
			}
		})
	}
}

func TestAlbumProductionAccountHashRemainsCompatible(t *testing.T) {
	// Frozen output from the previous production issuer/audience/subject algorithm.
	const expected = "4565c165a67c0659f8e8a2379cdd6e0921c56047a0296db36e01fa4faf330d26"
	if got := appleSubjectHash(albumAppleAudience, "existing-apple-user"); got != expected {
		t.Fatal("production accounts would become unreachable")
	}
	if appleSubjectHash(albumDevelopmentAudience, "existing-apple-user") == expected {
		t.Fatal("development and production account namespaces collide")
	}
	if uuid.NewSHA1(uuid.NameSpaceURL, []byte("cn.tellyouwhat.albums/storekit/v1:"+expected)).String() != "18e9dfea-9807-52fb-b18b-e3f5d927a2ea" {
		t.Fatal("existing StoreKit account-token namespace changed")
	}
	// These must fail before touching storage, including for already verified input.
	repository := &MySQLAccounts{bundleID: albumDevelopmentAudience}
	if _, err := repository.RegisterAppleLogin(context.Background(), AppleGrant{Identity: AppleIdentity{Audience: albumAppleAudience, Subject: "user", IssuedAt: time.Now()}, RefreshToken: "synthetic"}); !errors.Is(err, ErrAppleIdentity) {
		t.Fatal("repository accepted a different App's login")
	}
	if err := repository.applyAppleEvent(context.Background(), appleAccountEvent{Audience: albumAppleAudience, Type: "account-deleted", Subject: "user", EventTime: time.Now().Unix()}); !errors.Is(err, errAppleNotification) {
		t.Fatal("repository accepted a different App's deletion event")
	}
}

func TestAlbumGatewayBindsIdentityToDeployment(t *testing.T) {
	for _, deployment := range []string{"test", "production"} {
		t.Run(deployment, func(t *testing.T) {
			values := gatewayEnvironment(t)
			values["ALBUM_DEPLOYMENT_ENVIRONMENT"] = deployment
			want, opposite := albumDevelopmentAudience, albumAppleAudience
			values["ALBUM_API_HOST"] = "api-test.albums.tellyouwhat.cn"
			oppositeHost := "api.albums.tellyouwhat.cn"
			if deployment == "production" {
				values["ALBUM_DATABASE_DSN"] = "app:synthetic@tcp(localhost:3306)/albums_production"
				values["ALBUM_API_HOST"], oppositeHost = oppositeHost, values["ALBUM_API_HOST"]
				want, opposite = opposite, want
			}
			config, err := LoadGatewayConfig(func(k string) string { return values[k] })
			if err != nil || config.AppleCode.BundleID != want {
				t.Fatal("wrong deployment identity", err)
			}
			values["ALBUM_APPLE_BUNDLE_ID"] = opposite
			if _, err := LoadGatewayConfig(func(k string) string { return values[k] }); err == nil {
				t.Fatal("cross-environment Bundle ID override accepted")
			}
			values["ALBUM_APPLE_BUNDLE_ID"] = want
			values["ALBUM_API_HOST"] = oppositeHost
			if _, err := LoadGatewayConfig(func(k string) string { return values[k] }); err == nil {
				t.Fatal("cross-environment host accepted")
			}
			config.AppleCode.BundleID = opposite
			if _, err := NewGateway(context.Background(), config, nil); err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatal("direct gateway construction bypassed identity binding", err)
			}
		})
	}
}

func TestAlbumPushTopicFollowsIdentityIndependentlyOfAPNSEnvironment(t *testing.T) {
	for _, audience := range []string{albumAppleAudience, albumDevelopmentAudience} {
		for _, environment := range []string{"development", "production"} {
			t.Run(audience+"/"+environment, func(t *testing.T) {
				config, _, claim, now := apnsFixture(t)
				config.BundleID, config.Environment = audience, environment
				called := false
				sender, err := newAlbumAPNS(config, appleTestTransport(func(r *http.Request) (*http.Response, error) {
					called = true
					wantHost := "api.push.apple.com"
					if environment == "development" {
						wantHost = "api.sandbox.push.apple.com"
					}
					if r.URL.Host != wantHost || r.Header.Get("apns-topic") != audience {
						t.Fatal("push routed to the wrong app/environment")
					}
					response := appleResponse("")
					response.ProtoMajor = 2
					response.Header.Set("apns-id", r.Header.Get("apns-id"))
					return response, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				sender.now = func() time.Time { return now }
				if result, err := sender.sendRetention(context.Background(), strings.Repeat("ab", 32), claim, uuid.NewString()); err != nil || result.Status != "accepted" || !called {
					t.Fatal("push identity test failed", err)
				}
			})
		}
	}
}

func TestAlbumSubscriptionVerifierRejectsOtherBundle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newAlbumReceiptFixture(t, now)
	for _, audience := range []string{albumAppleAudience, albumDevelopmentAudience} {
		config := &AlbumSubscriptionConfig{BundleID: audience, Environment: "Sandbox", IssuerID: uuid.NewString(), KeyID: "ALBUMSKEY1", SigningKey: albumTestECKey(t), Roots: fixture.roots}
		service, err := newConfiguredAlbumSubscriptions(NewMySQLUploads(nil).WithSubscriptionEnvironment("Sandbox"), config, appleTestTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("verification must not require a provider request")
			return nil, errors.New("unexpected request")
		}))
		if err != nil {
			t.Fatal(err)
		}
		for _, signedAudience := range []string{albumAppleAudience, albumDevelopmentAudience} {
			payload := fixture.payload(uuid.NewString())
			payload["bundleId"] = signedAudience
			_, err := service.resolver.verifier.VerifyTransaction(fixture.sign(t, payload))
			if (err == nil) != (audience == signedAudience) {
				t.Fatalf("subscription bundle binding: want %s, receipt %s, error %v", audience, signedAudience, err)
			}
		}
	}
}

func TestAlbumInvalidIdentityAndMismatchedComponentsFailClosed(t *testing.T) {
	values := gatewayEnvironment(t)
	config, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{"", "cn.tellyouwhat.healthapp", "cn.tellyouwhat.albums.other"} {
		v := NewAppleIdentityVerifier(audience, appleTestTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid configuration reached Apple")
			return nil, errors.New("unexpected request")
		}))
		if _, err := v.Verify(context.Background(), "synthetic", strings.Repeat("a", 64)); !errors.Is(err, ErrAppleIdentity) {
			t.Fatal("invalid verifier identity accepted")
		}
		if _, err := v.verifyAccountEvent(context.Background(), "synthetic"); !errors.Is(err, errAppleNotification) {
			t.Fatal("invalid notification verifier identity accepted")
		}
	}
	if _, err := NewAppleCodeExchanger(config.AppleCode, NewAppleIdentityVerifier(albumAppleAudience, nil), nil); err == nil {
		t.Fatal("code exchanger and identity verifier can disagree")
	}
	for _, component := range []string{"subscription", "push"} {
		c := config
		if component == "subscription" {
			c.Subscriptions = &AlbumSubscriptionConfig{BundleID: albumAppleAudience}
		} else {
			c.RetentionAPNS = &albumAPNSConfig{BundleID: albumAppleAudience}
		}
		if _, err := NewGateway(context.Background(), c, nil); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatal("gateway accepted a differently scoped component", err)
		}
	}
}
