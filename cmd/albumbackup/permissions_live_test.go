//go:build albums_live

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

const permissionProbeScope = "albums-backup-permission-probe-v1"

type permissionIntent struct {
	Scope, Key, UploadID, ReceiptUploadID, Version string
}

type permissionCheck struct {
	Name, Code string
	Status     int
	Passed     bool
}

// Only this opt-in fixture owns these keys. Failed runs retain their intent for cleanup.
func TestLiveBackupPermissionBoundaries(t *testing.T) {
	root := os.Getenv("ALBUM_BACKUP_PERMISSION_PROBE_DIR")
	if root == "" {
		t.Skip("real COS permission probe requires an explicit private directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !filepath.IsAbs(root) {
		t.Fatal("private absolute probe directory required")
	}
	lock, err := os.OpenFile(filepath.Join(root, "probe.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal("probe lease unavailable")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("probe is already active; preserved")
	}
	read := func(path string, value any) {
		f, e := privateFile(path)
		if e != nil {
			t.Fatal("private probe input unavailable")
		}
		defer f.Close()
		decoder := json.NewDecoder(io.LimitReader(f, 65537))
		decoder.DisallowUnknownFields()
		if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
			t.Fatal("invalid private probe input")
		}
	}
	write := func(name string, value any) error {
		data, e := json.MarshalIndent(value, "", "  ")
		if e != nil {
			return e
		}
		path := filepath.Join(root, name+".tmp")
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		defer os.Remove(path)
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		closed := f.Close()
		if e != nil {
			return e
		}
		if closed != nil {
			return closed
		}
		if e = os.Rename(path, filepath.Join(root, name)); e != nil {
			return e
		}
		directory, e := os.Open(root)
		if e != nil {
			return e
		}
		defer directory.Close()
		return directory.Sync()
	}
	var selected struct{ Scope, Writer, Cleanup string }
	read(filepath.Join(root, "probe.json"), &selected)
	if selected.Scope != permissionProbeScope || !filepath.IsAbs(selected.Writer) || !filepath.IsAbs(selected.Cleanup) || selected.Writer == selected.Cleanup {
		t.Fatal("explicit separate backup credentials required")
	}
	var writer, cleanup config
	read(selected.Writer, &writer)
	read(selected.Cleanup, &cleanup)
	if writer.Bucket != "tellyouwhat-albums-backups-test-1251995834" || writer.Region != "ap-guangzhou" ||
		cleanup.Bucket != writer.Bucket || cleanup.Region != writer.Region || writer.SecretID == cleanup.SecretID {
		t.Fatal("probe is restricted to the approved test bucket and two identities")
	}
	payload := []byte("Disposable synthetic Albums backup permission fixture.\n")
	digest := sha256.Sum256(payload)
	hash := hex.EncodeToString(digest[:])
	intent := permissionIntent{Scope: permissionProbeScope}
	sign := func(value permissionIntent) []byte {
		encoded, _ := json.Marshal(value)
		mac := hmac.New(sha256.New, []byte(writer.SecretKey))
		mac.Write(encoded)
		return mac.Sum(nil)
	}
	type envelope struct {
		Intent permissionIntent
		HMAC   []byte
	}
	saveIntent := func() error { return write("intent.json", envelope{intent, sign(intent)}) }
	intentPath := filepath.Join(root, "intent.json")
	_, existing := os.Lstat(intentPath)
	if existing != nil && !errors.Is(existing, os.ErrNotExist) {
		t.Fatal("cannot inspect probe intent")
	}
	if existing == nil {
		var saved envelope
		read(intentPath, &saved)
		intent = saved.Intent
		if !hmac.Equal(saved.HMAC, sign(intent)) || intent.Scope != permissionProbeScope || !keyPattern.MatchString(intent.Key) || filepath.Ext(intent.Key) != ".enc" {
			t.Fatal("unexpected existing probe intent; preserved")
		}
	} else {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal("probe nonce unavailable")
		}
		intent.Key = recoveryPrefix + hex.EncodeToString(nonce[:]) + ".enc"
		if saveIntent() != nil {
			t.Fatal("cannot persist probe intent")
		}
	}
	checks := []permissionCheck{}
	report := map[string]any{"scope": permissionProbeScope, "bucket": writer.Bucket, "region": writer.Region,
		"key": intent.Key, "checks": checks, "passed": false, "cleanup_confirmed": false}
	defer func() {
		// The fixed payload binds cleanup to this test's own versions, including unexpected PUTs.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		removed, err := expire(ctx, cleanup, request{Operation: "expire", Key: intent.Key, Bucket: cleanup.Bucket,
			Region: cleanup.Region, Bytes: int64(len(payload)), SHA256: hash, ReceiptBytes: int64(len(payload)),
			ReceiptSHA256: hash, Protocol: multipartProtocol, UploadID: intent.UploadID, ReceiptUploadID: intent.ReceiptUploadID}, nil)
		report["checks"] = checks
		report["cleanup"] = removed
		report["cleanup_confirmed"] = err == nil && removed.Empty && removed.UploadsEmpty
		if err != nil {
			t.Error("owned fixture cleanup incomplete; intent retained for recovery")
		}
		report["local_payload_created"] = false
		report["passed"] = !t.Failed() && existing != nil && err == nil
		name := "report.json"
		if existing == nil {
			name = "recovery-" + time.Now().UTC().Format("20060102T150405.000000000") + ".json"
		}
		if write(name, report) != nil {
			t.Error("probe report could not be saved")
		}
	}()
	if existing == nil {
		t.Fatal("existing probe is cleanup-only; use a new directory for a new matrix")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	w, err := newClient(writer, nil)
	if err != nil {
		t.Fatal("writer client unavailable")
	}
	c, err := newClient(cleanup, nil)
	if err != nil {
		t.Fatal("cleanup client unavailable")
	}
	upload, err := initiate(ctx, writer, request{Operation: "initiate", Key: intent.Key, Bucket: writer.Bucket, Region: writer.Region}, nil)
	if err != nil {
		t.Fatal("fixture initiation failed; intent retained")
	}
	intent.UploadID = upload.UploadID
	if saveIntent() != nil {
		t.Fatal("cannot persist upload session")
	}
	initialPart, err := w.Object.UploadPart(ctx, intent.Key, intent.UploadID, 1, bytes.NewReader(payload), &cos.ObjectUploadPartOptions{ContentLength: int64(len(payload))})
	if err != nil || initialPart == nil {
		closeResponse(initialPart)
		t.Fatal("fixture part unavailable")
	}
	initialETag := initialPart.Header.Get("ETag")
	closeResponse(initialPart)
	_, completed, err := w.Object.CompleteMultipartUpload(ctx, intent.Key, intent.UploadID, &cos.CompleteMultipartUploadOptions{Parts: []cos.Object{{PartNumber: 1, ETag: initialETag}}})
	if err != nil || completed == nil {
		closeResponse(completed)
		t.Fatal("fixture completion unavailable")
	}
	intent.Version = completed.Header.Get("x-cos-version-id")
	closeResponse(completed)
	if saveIntent() != nil {
		t.Fatal("cannot persist fixture version")
	}
	if !validVersion(intent.Version) || verifyObject(ctx, w, intent.Key, intent.Version, hash, int64(len(payload))) != nil {
		t.Fatal("fixture exact version verification failed")
	}
	receiptKey := strings.TrimSuffix(intent.Key, ".enc") + ".receipt.json"
	active, err := initiate(ctx, writer, request{Operation: "initiate", Key: receiptKey, Bucket: writer.Bucket, Region: writer.Region}, nil)
	if err != nil {
		t.Fatal("active multipart fixture unavailable")
	}
	intent.ReceiptUploadID = active.UploadID
	if saveIntent() != nil {
		t.Fatal("cannot persist active fixture session")
	}
	part, err := w.Object.UploadPart(ctx, receiptKey, active.UploadID, 1, bytes.NewReader(payload), &cos.ObjectUploadPartOptions{ContentLength: int64(len(payload))})
	if err != nil || part == nil {
		closeResponse(part)
		t.Fatal("multipart fixture part unavailable")
	}
	etag := part.Header.Get("ETag")
	closeResponse(part)
	denied := func(name string, response *cos.Response, err error) {
		result := permissionCheck{Name: name}
		if response != nil {
			result.Status = response.StatusCode
		}
		var provider *cos.ErrorResponse
		if errors.As(err, &provider) && provider.Code == "AccessDenied" {
			result.Code = "AccessDenied"
		}
		closeResponse(response)
		result.Passed = result.Status == http.StatusForbidden && result.Code == "AccessDenied"
		checks = append(checks, result)
		if !result.Passed {
			t.Errorf("%s did not return the required 403 AccessDenied (HTTP %d)", name, result.Status)
		}
	}
	query := url.Values{"versionId": {intent.Version}}
	r, e := w.Object.Delete(ctx, intent.Key, &cos.ObjectDeleteOptions{XOptionQuery: &query})
	denied("writer_version_delete", r, e)
	r, e = w.Object.Delete(ctx, intent.Key, nil)
	denied("writer_delete_marker", r, e)
	r, e = w.Object.Put(ctx, intent.Key, bytes.NewReader(payload), nil)
	denied("writer_ordinary_put", r, e)
	r, e = c.Object.Put(ctx, intent.Key, bytes.NewReader(payload), nil)
	denied("cleanup_ordinary_put", r, e)
	r, e = w.Object.AbortMultipartUpload(ctx, receiptKey, active.UploadID)
	denied("writer_abort_multipart", r, e)
	_, r, e = c.Object.InitiateMultipartUpload(ctx, receiptKey, nil)
	denied("cleanup_initiate_multipart", r, e)
	r, e = c.Object.UploadPart(ctx, receiptKey, active.UploadID, 2, bytes.NewReader(payload), &cos.ObjectUploadPartOptions{ContentLength: int64(len(payload))})
	denied("cleanup_upload_part", r, e)
	_, r, e = c.Object.CompleteMultipartUpload(ctx, receiptKey, active.UploadID, &cos.CompleteMultipartUploadOptions{Parts: []cos.Object{{PartNumber: 1, ETag: etag}}})
	denied("cleanup_complete_multipart", r, e)
	r, e = c.Object.Delete(ctx, intent.Key, nil)
	denied("cleanup_delete_marker", r, e)
	query = url.Values{"versionId": {"null"}}
	r, e = c.Object.Delete(ctx, intent.Key, &cos.ObjectDeleteOptions{XOptionQuery: &query})
	denied("cleanup_null_version_delete", r, e)
	for _, client := range []struct {
		name string
		cos  *cos.Client
		cfg  config
	}{{"writer", w, writer}, {"cleanup", c, cleanup}} {
		r, e = client.cos.Object.Get(ctx, "albums/outside-permission-probe/"+filepath.Base(intent.Key), nil)
		denied(client.name+"_outside_prefix_read", r, e)
		_, r, e = client.cos.Bucket.GetACL(ctx)
		denied(client.name+"_bucket_acl_read", r, e)
		_, r, e = client.cos.Bucket.GetObjectVersions(ctx, &cos.BucketGetObjectVersionsOptions{Prefix: "albums/outside-permission-probe/", MaxKeys: 1})
		denied(client.name+"_outside_prefix_versions", r, e)
		for _, target := range []struct{ name, bucket string }{
			{"test_media", "tellyouwhat-albums-1251995834"},
			{"production_media", "tellyouwhat-albums-production-1251995834"},
		} {
			foreign := client.cfg
			foreign.Bucket = target.bucket
			reader, err := newClient(foreign, nil)
			if err != nil {
				t.Fatal("cross-bucket denial client unavailable")
			}
			// Only read-only probes at this run's random key; never enumerate user media or mutate another bucket.
			r, e = reader.Object.Get(ctx, intent.Key, nil)
			denied(client.name+"_"+target.name+"_read", r, e)
			_, r, e = reader.Bucket.GetObjectVersions(ctx, &cos.BucketGetObjectVersionsOptions{Prefix: intent.Key, MaxKeys: 1})
			denied(client.name+"_"+target.name+"_versions", r, e)
		}
	}
	if verifyObject(ctx, w, intent.Key, intent.Version, hash, int64(len(payload))) != nil {
		t.Error("initial exact fixture version was not preserved")
	}
}
