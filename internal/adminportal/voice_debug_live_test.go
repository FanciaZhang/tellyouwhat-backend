package adminportal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/adminui"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/prompteval"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

// Real model access has a separate opt-in and bounded budget. The only account
// is a synthetic Journal operator in a disposable DB; no test auth ships.
func TestLiveVoiceDebugBrowser(t *testing.T) {
	file := os.Getenv("JOURNAL_VOICE_DEBUG_LIVE_CONFIG")
	if file == "" || os.Getenv("JOURNAL_VOICE_DEBUG_LIVE_EXECUTE") != "1" {
		t.Skip("explicit synthetic live browser acceptance only")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("private configuration unavailable")
	}
	settings := map[string]string{}
	if json.Unmarshal(raw, &settings) != nil {
		t.Fatal("invalid private configuration")
	}
	maximum, err := strconv.ParseInt(os.Getenv("JOURNAL_VOICE_DEBUG_MAX_NANOS"), 10, 64)
	if err != nil || maximum <= 0 {
		t.Fatal("explicit remaining budget required")
	}
	evidence := os.Getenv("JOURNAL_VOICE_DEBUG_EVIDENCE_DIR")
	if !filepath.IsAbs(evidence) {
		t.Fatal("persistent evidence directory required")
	}
	if err = os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	actor := uuid.NewString()
	if _, err = db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role)VALUES(?,?,?,'operator')`, actor, []byte(actor), "Synthetic voice operator"); err != nil {
		t.Fatal(err)
	}
	model := settings["JOURNAL_VOICE_MODEL_ID"]
	if model == "" {
		model = settings["JOURNAL_ARK_PRO_MODEL_ID"]
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 9600000000, OutputNanosPerMillionTokens: 48000000000}
	speechPrice := costcontrol.DurationPrice{NanosPerHour: 4500000000}
	policy := promptconfig.Defaults(settings["JOURNAL_ARK_LITE_MODEL_ID"], settings["JOURNAL_ARK_PRO_MODEL_ID"], model, 90)["journal"]
	policy.Journal.Voice.Parameters.MaxOutputTokens = 512
	policy.Journal.Voice.PolishPrompt = "理顺句子，保留同行人、散步、热茶等具体细节。"
	prompts := &promptconfig.Store{DB: db}
	if err = prompts.Initialize(ctx, "journal", policy, time.Now()); err != nil {
		t.Fatal(err)
	}
	cipher, _ := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	store := prompteval.Store{DB: db, Cipher: cipher, Limits: costcontrol.Limits{MonthlyBudgetNanos: maximum, MaxConcurrent: 2, LeaseDuration: time.Minute}}
	sessions := adminauth.NewMemoryStateStore(time.Now)
	repo := &aiAuthFixture{user: adminauth.User{ID: actor, Role: adminauth.RoleOperator, Status: adminauth.UserStatusActive, AppIDs: []string{"journal"}}}
	router := gin.New()
	host := httptest.NewUnstartedServer(router)
	defer host.Close()
	origin := "https://" + host.Listener.Addr().String()
	// Map only this local TLS listener to the synthetic authentication RP origin.
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Origin") == origin {
			c.Request.Header.Set("Origin", "https://admin.example.com")
		}
	})
	auth, err := adminauth.NewService(repo, sessions, adminauth.Config{RPID: "admin.example.com", Origin: "https://admin.example.com", AppIDs: []string{"journal"}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	csrf := uuid.NewString()
	now := time.Now()
	if err = sessions.PutSession(ctx, adminauth.TokenHash(token), adminauth.Session{UserID: actor, CSRFToken: csrf, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}, time.Hour); err != nil {
		t.Fatal(err)
	}
	configured := map[string]costcontrol.TokenPrice{}
	for _, id := range []string{settings["JOURNAL_ARK_LITE_MODEL_ID"], settings["JOURNAL_ARK_PRO_MODEL_ID"], model} {
		configured[id] = price
	}
	s := &Server{auth: auth, now: time.Now, limiter: newRateLimiter(time.Now), config: Config{Environment: "development", Apps: []AdminApp{{ID: "journal", DisplayName: "告你手记"}}, WritesEnabled: true, ConfiguredPromptModels: configured, Prompts: prompts, Evaluations: &store, EvaluationSpeechPrice: speechPrice, PreviewSigningKey: bytes.Repeat([]byte{2}, 32)}}
	router.Use(s.securityAndRateLimit())
	router.Use(limitAdminRequestBody())
	adminhttpapi.RegisterHandlers(router, &adminHTTPServer{Server: s, Service: auth})
	router.NoRoute(func(c *gin.Context) { adminui.Render(c, true) })
	host.StartTLS()
	asr := voice.ASRConfig{URL: settings["JOURNAL_VOICE_ASR_URL"], ResourceID: settings["JOURNAL_VOICE_ASR_RESOURCE_ID"], APIKey: settings["JOURNAL_VOICE_ASR_API_KEY"], AppKey: settings["JOURNAL_VOICE_ASR_APP_KEY"], AccessKey: settings["JOURNAL_VOICE_ASR_ACCESS_KEY"]}
	engine := &prompteval.Engine{Store: store, ASR: asr, SpeechPrice: speechPrice, Price: price, Provider: provider.Config{BaseURL: settings["JOURNAL_ARK_BASE_URL"], APIKey: settings["JOURNAL_ARK_API_KEY"]}}
	done := make(chan struct{})
	go func() { defer close(done); engine.Run(ctx) }()
	defer func() { cancel(); <-done }()
	defer func() {
		var accounted int64
		if err := db.QueryRow(`SELECT COALESCE(SUM(IF(status='settled',actual_nanos,reserved_nanos)),0) FROM ai_cost_attempts`).Scan(&accounted); err != nil {
			t.Error(err)
		}
		receipt, _ := json.MarshalIndent(map[string]any{"synthetic": true, "actor": "isolated Journal operator", "accountedNanos": accounted, "limitNanos": maximum, "passed": !t.Failed()}, "", "  ")
		if err := os.WriteFile(filepath.Join(evidence, "live-browser-cost.json"), receipt, 0600); err != nil {
			t.Error(err)
		}
	}()
	pcm, err := os.ReadFile("../prompteval/fixtures/river.pcm")
	if err != nil {
		t.Fatal(err)
	}
	wav := make([]byte, 44+len(pcm))
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	copy(wav[44:], pcm)
	audio := filepath.Join(t.TempDir(), "synthetic-river.wav")
	if err = os.WriteFile(audio, wav, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "node", "../../scripts/voice-lab-live-ui.mjs")
	command.Env = append(os.Environ(), "VOICE_LIVE_URL="+host.URL, "VOICE_LIVE_SESSION="+token, "VOICE_LIVE_CSRF="+csrf, "VOICE_LIVE_ACTOR="+actor, "VOICE_LIVE_AUDIO="+audio, "VOICE_LIVE_EVIDENCE="+evidence)
	log, err := command.CombinedOutput()
	if writeErr := os.WriteFile(filepath.Join(evidence, "browser.log"), log, 0600); writeErr != nil {
		t.Error(writeErr)
	}
	if err != nil {
		t.Fatal("live browser journey failed; inspect preserved browser log", err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM prompt_eval_runs WHERE actor_id=? AND status='completed'`, actor).Scan(&count); err != nil || count != 2 {
		t.Fatal("ASR and finalization were not persisted", err, count)
	}
}
