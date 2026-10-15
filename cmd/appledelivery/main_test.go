package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/appledelivery"
)

func TestPublishSavesPrivateReceiptWithoutOverwritingFiles(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	if err := os.Mkdir(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte("synthetic transport fixture, not a signed IPA")
	hash := sha256.Sum256(body)
	c := appledelivery.Candidate{BundleID: "cn.tellyouwhat.albums.dev", Source: strings.Repeat("a", 40), Version: "0.1.0", Build: "1", Digest: hex.EncodeToString(hash[:]), Size: int64(len(body)), State: "verified-local-candidate"}
	metadata, _ := json.Marshal(c)
	_ = os.WriteFile(filepath.Join(candidate, "candidate.json"), metadata, 0600)
	_ = os.WriteFile(filepath.Join(candidate, "development.ipa"), body, 0600)
	receipts := filepath.Join(root, "receipts")
	if err := os.Mkdir(receipts, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(receipts, "receipt.json")
	args := []string{"publish", "--data", filepath.Join(root, "data"), "--candidate", candidate, "--receipt", receipt, "--origin", "https://install.example", "--ttl", "1h"}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(receipt)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt not private")
	}
	data, _ := os.ReadFile(receipt)
	var saved appledelivery.Receipt
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Source != c.Source || saved.Digest != c.Digest || !strings.HasPrefix(saved.URL, "https://install.example/d/") {
		t.Fatal("receipt lost its source binding")
	}
	if err := run(args); err == nil {
		t.Fatal("existing receipt overwritten")
	}
	after, _ := os.ReadFile(receipt)
	if string(after) != string(data) {
		t.Fatal("receipt modified")
	}
}

func TestIndependentPublisherProcessesShareTheirFileLock(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "appledelivery")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	candidate := filepath.Join(root, "candidate")
	receipts := filepath.Join(root, "receipts")
	for _, dir := range []string{candidate, receipts} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("synthetic concurrent publisher transport fixture")
	hash := sha256.Sum256(body)
	c := appledelivery.Candidate{BundleID: "cn.tellyouwhat.birthday.dev", Source: strings.Repeat("a", 40), Version: "1.0", Build: "2", Digest: hex.EncodeToString(hash[:]), Size: int64(len(body)), State: "verified-local-candidate"}
	metadata, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(candidate, "candidate.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "development.ipa"), body, 0600); err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{}
	for i := 0; i < 4; i++ {
		command := exec.CommandContext(ctx, binary, "publish", "--data", filepath.Join(root, "data"), "--candidate", candidate, "--origin", "https://install.example", "--receipt", filepath.Join(receipts, fmt.Sprintf("receipt-%d.json", i)))
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Error("publisher process failed", err)
		}
	}
	entries, err := os.ReadDir(receipts)
	if err != nil || len(entries) != 4 {
		t.Fatal("concurrent receipts missing", len(entries), err)
	}
}

func TestJournalConnectionCommandWritesOnlyEncryptedPrivateReceipt(t *testing.T) {
	root := t.TempDir()
	candidate, receipts := filepath.Join(root, "candidate"), filepath.Join(root, "receipts")
	for _, dir := range []string{candidate, receipts} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("synthetic Journal transport fixture; not a signed IPA")
	hash := sha256.Sum256(body)
	c := appledelivery.Candidate{BundleID: "cn.tellyouwhat.journalapp.dev", Source: strings.Repeat("a", 40), Version: "1.0", Build: "19", Digest: hex.EncodeToString(hash[:]), Size: int64(len(body)), State: "verified-local-candidate"}
	metadata, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(candidate, "candidate.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	s, err := appledelivery.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(c, strings.NewReader(string(body)), "https://install.example", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	profile := filepath.Join(root, "connection.json")
	secret := strings.Repeat("s", 43)
	private, _ := json.Marshal(map[string]any{"version": 1, "origin": "https://api-test.journal.tellyouwhat.cn", "serviceProtocol": "subscription-v2", "voiceProtocol": "journal-voice-v36", "token": secret})
	if err := os.WriteFile(profile, private, 0600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(receipts, "connection.json")
	args := []string{"journal-connection", "--data", data, "--candidate", candidate, "--journal-connection", profile, "--origin", "https://install.example", "--ttl", "15m", "--receipt", receipt}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(receipt)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt not private")
	}
	payload, _ := os.ReadFile(receipt)
	var r appledelivery.Receipt
	if json.Unmarshal(payload, &r) != nil || r.Source != c.Source || !strings.Contains(r.URL, "/d/connection/") || strings.Contains(string(payload), secret) {
		t.Fatal("invalid private receipt")
	}
	if err := run(args); err == nil {
		t.Fatal("existing private receipt overwritten")
	}
	invalid := append([]string(nil), args...)
	invalid[10], invalid[12] = "24h", filepath.Join(receipts, "invalid.json")
	if err := run(invalid); err == nil {
		t.Fatal("long-lived connection accepted")
	}
	if _, err := os.Stat(invalid[12]); !os.IsNotExist(err) {
		t.Fatal("invalid lease created a receipt")
	}
	if err := run([]string{"cleanup", "--data", data, "--journal-connection", profile}); err == nil {
		t.Fatal("connection flag accepted by cleanup")
	}
}
