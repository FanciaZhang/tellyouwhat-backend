//go:build albums_live

package albums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

const liveTrashProcessScope = "albums-owned-trash-process-v1"

type liveTrashProcessFixture struct {
	Scope, Owner, Asset, Database, Socket, Run string
	Clock                                      time.Time
}

type liveTrashMutation struct{ Kind, Key, VersionOrUpload string }
type liveTrashProcessResult struct {
	Passed          bool
	Settled, Aborts int
	Deletes         []liveTrashVersion
	Receipts        int
	Used, Reserved  int64
}

func liveTrashPendingMutation(p objectPurge, m liveTrashMutation) bool {
	if p.Plan == nil || p.Index < 0 || p.Index >= len(p.Targets) || p.Targets[p.Index].Key != m.Key || p.ActionIndex < 0 {
		return false
	}
	if len(p.Plan.Uploads) > 0 {
		return m.Kind == "abort" && p.ActionIndex < len(p.Plan.Uploads) && p.Plan.Uploads[p.ActionIndex] == m.VersionOrUpload
	}
	return m.Kind == "delete" && p.ActionIndex < len(p.Plan.Versions) && p.Plan.Versions[p.ActionIndex].VersionID == m.VersionOrUpload
}

func TestLiveTrashPendingMutation(t *testing.T) {
	for _, kind := range []string{"abort", "delete"} {
		t.Run(kind, func(t *testing.T) {
			p := objectPurge{Targets: []purgeTarget{{"owned-key"}}, Plan: &purgePlan{}}
			if kind == "abort" {
				p.Plan.Uploads = []string{"owned-id"}
			} else {
				p.Plan.Versions = []DerivedObjectVersion{{VersionID: "owned-id"}}
			}
			m := liveTrashMutation{kind, "owned-key", "owned-id"}
			if !liveTrashPendingMutation(p, m) {
				t.Fatal("pending operation rejected")
			}
			for _, other := range []liveTrashMutation{{kind, "neighbor", "owned-id"}, {kind, "owned-key", "other-id"}, {"other", "owned-key", "owned-id"}} {
				if liveTrashPendingMutation(p, other) {
					t.Fatal("another operation accepted as the held action")
				}
			}
			p.ActionIndex++
			if liveTrashPendingMutation(p, m) {
				t.Fatal("already acknowledged action accepted")
			}
		})
	}
}

func liveTrashProcessStorage(cleaner *COSObjects, in liveTrashIntent) *liveTrashStorage {
	s := &liveTrashStorage{cleaner: cleaner, owner: in.Owner, allowed: map[string]map[string]bool{}, uploads: map[string]map[string]bool{}}
	for _, key := range in.Keys[:2] {
		s.allowed[key], s.uploads[key] = map[string]bool{}, map[string]bool{}
	}
	for _, v := range in.Versions[:3] {
		s.allowed[v.Key][v.VersionID] = true
	}
	s.uploads[in.Keys[1]][in.Multipart] = true
	return s
}

func liveTrashProcessDatabase(raw string, f liveTrashProcessFixture) (*mysql.Config, error) {
	c, err := mysql.ParseDSN(raw)
	if err != nil || f.Scope != liveTrashProcessScope || !validOwner(f.Owner) || !resourceIDPattern.MatchString(f.Asset) ||
		len(f.Run) != 8 || strings.Trim(f.Run, "0123456789abcdef") != "" || !albumTestDatabasePattern.MatchString(f.Database) ||
		!strings.HasPrefix(f.Database, "albums_gateway"+f.Run) || f.Clock.IsZero() || c.Net != "unix" || c.Addr != f.Socket ||
		c.DBName != f.Database || !strings.HasPrefix(c.User, "algw_"+f.Run) || c.Passwd == "" {
		return nil, ErrInvalidManifest
	}
	info, err := os.Lstat(filepath.Dir(f.Socket))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalidManifest
	}
	return c, nil
}

func TestLiveTrashProcessDatabaseScope(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	f := liveTrashProcessFixture{Scope: liveTrashProcessScope, Owner: uuid.NewString(), Asset: "trash-" + uuid.NewString(), Run: "1234abcd", Clock: time.Now(), Socket: filepath.Join(root, "mysql.sock"), Database: "albums_gateway1234abcd" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"}
	c := mysql.NewConfig()
	c.Net, c.Addr, c.DBName, c.User, c.Passwd = "unix", f.Socket, f.Database, "algw_1234abcd_owned_test", "local-synthetic-password"
	if _, err := liveTrashProcessDatabase(c.FormatDSN(), f); err != nil {
		t.Fatal("valid parent-owned fixture refused", err)
	}
	for name, change := range map[string]func(*mysql.Config, *liveTrashProcessFixture){
		"network":      func(c *mysql.Config, _ *liveTrashProcessFixture) { c.Net = "tcp" },
		"other schema": func(c *mysql.Config, _ *liveTrashProcessFixture) { c.DBName = "other_test" },
		"admin user":   func(c *mysql.Config, _ *liveTrashProcessFixture) { c.User = "root" },
		"wrong socket": func(c *mysql.Config, _ *liveTrashProcessFixture) { c.Addr += "-other" },
		"no password":  func(c *mysql.Config, _ *liveTrashProcessFixture) { c.Passwd = "" },
		"other run":    func(_ *mysql.Config, f *liveTrashProcessFixture) { f.Run = "eeeeeeee" },
		"other scope":  func(_ *mysql.Config, f *liveTrashProcessFixture) { f.Scope = "other" },
		"no owner":     func(_ *mysql.Config, f *liveTrashProcessFixture) { f.Owner = "" },
		"no clock":     func(_ *mysql.Config, f *liveTrashProcessFixture) { f.Clock = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			copyConfig, copyFixture := c.Clone(), f
			change(copyConfig, &copyFixture)
			if _, err := liveTrashProcessDatabase(copyConfig.FormatDSN(), copyFixture); err == nil {
				t.Fatal("out-of-scope child connection accepted")
			}
		})
	}
}

// The parent retains the ownership lease and the database until every child is
// reaped. Children only borrow the parent's exact, frozen synthetic inventory.
func TestLiveCOSTrashWorkerProcessChild(t *testing.T) {
	stage := os.Getenv("ALBUM_TRASH_PROCESS_CHILD")
	if stage == "" {
		t.Skip("only the owned process-recovery parent may start this child")
	}
	offset, ok := map[string]int{"abort": 0, "delete": 11, "finish": 22, "verify": 33}[stage]
	if !ok || os.Getenv("ALBUM_TRASH_COS_WORKER_PROBE") != "1" || os.Getenv("MYSQL_TEST_ADMIN_DSN") != "" {
		t.Fatal("invalid isolated process child")
	}
	reader, root, ctx := liveRoundtripFixture(t)
	lease := os.NewFile(4, "inherited-ownership-lease")
	defer lease.Close()
	lockInfo, lockErr := lease.Stat()
	pathInfo, pathErr := os.Lstat(filepath.Join(root, ".completion.lock"))
	if lockErr != nil || pathErr != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0077 != 0 || !os.SameFile(lockInfo, pathInfo) || syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("inherited parent ownership lease required")
	}
	in := liveTrashLoad(t, root)
	var fixture liveTrashProcessFixture
	roundtripRead(t, root, "process-fixture.json", &fixture)
	if fixture.Owner != in.Owner || fixture.Asset != in.Asset {
		t.Fatal("process fixture ownership differs")
	}
	config, err := liveTrashProcessDatabase(os.Getenv("ALBUM_TRASH_PROCESS_DSN"), fixture)
	if err != nil {
		t.Fatal("private parent-owned schema required")
	}
	db, err := mysqlstore.Open(ctx, config.FormatDSN())
	if err != nil {
		t.Fatal("private child database unavailable")
	}
	defer db.Close()
	storage := liveTrashProcessStorage(completionProbeCleaner(t), in)
	notify := os.NewFile(3, "owned-parent-notify")
	defer notify.Close()
	mutations := 0
	storage.afterMutation = func(kind, key, id string) {
		mutations++
		m := liveTrashMutation{kind, key, id}
		photoMultipartProbeCheckpoint(t, root, fmt.Sprintf("process-%s-mutation-%d.json", stage, mutations), m)
		if kind == stage {
			if json.NewEncoder(notify).Encode(m) != nil {
				t.Fatal("parent notification unavailable; retain intent")
			}
			<-ctx.Done()
			t.Fatal("parent did not terminate the held child")
		}
	}
	now := fixture.Clock.Add(time.Duration(offset) * time.Minute)
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	r.productionSandboxQuota = 1_000_000_000
	w := &retentionCleanupWorker{repository: r, storage: storage, batchSize: 10, now: func() time.Time { return now }, trashEnabled: true}
	settled := 0
	for pass := 0; pass < 16; pass++ {
		b, err := w.runOnce(ctx)
		if err != nil || b.Deferred != 0 {
			t.Fatal("process recovery failed; retain intent", err)
		}
		settled += b.Settled
		if stage == "verify" || settled != 0 {
			break
		}
		now = now.Add(6 * time.Second)
	}
	if stage == "abort" || stage == "delete" || stage == "finish" && settled != 1 || stage == "verify" && (settled != 0 || storage.deletes != 0 || storage.aborts != 0) {
		t.Fatal("expected process boundary or idempotent settlement not reached")
	}
	used, reserved := photoJournalBalance(t, db, in.Owner)
	var receipts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=? AND reason='trash'`, in.Owner, in.Asset).Scan(&receipts); err != nil || receipts != 1 || used != 0 || reserved != 0 {
		t.Fatal("process recovery changed settlement cardinality or quota")
	}
	liveTrashReadVersion(t, ctx, reader, in.Versions[3])
	photoMultipartProbeCheckpoint(t, root, "process-"+stage+"-result.json", liveTrashProcessResult{true, settled, storage.aborts, storage.deleteAttempts, receipts, used, reserved})
}

func runLiveTrashProcessRecovery(t *testing.T, ctx context.Context, root string, lease *os.File, reader, cleaner *COSObjects, in liveTrashIntent) {
	t.Helper()
	admin, err := mysql.ParseDSN(os.Getenv("MYSQL_TEST_ADMIN_DSN"))
	if err != nil || admin.Net != "unix" {
		t.Fatal("socket-only private MySQL required")
	}
	storage, markers := liveTrashInventory(t, ctx, reader, cleaner, in)
	if markers != 0 || len(storage.allowed[in.Keys[0]]) != 2 || len(storage.allowed[in.Keys[1]]) != 1 || !storage.uploads[in.Keys[1]][in.Multipart] {
		t.Fatal("fresh prepared process inventory required")
	}
	db, dsn := gatewayTestDatabase(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := seedLiveTrashDatabase(t, ctx, db, in)
	config, _ := mysql.ParseDSN(dsn)
	fixture := liveTrashProcessFixture{liveTrashProcessScope, in.Owner, in.Asset, config.DBName, config.Addr, os.Getenv("ALBUM_TEST_RUN_ID"), clock}
	if _, err := liveTrashProcessDatabase(dsn, fixture); err != nil {
		t.Fatal("unsafe parent fixture")
	}
	photoMultipartProbeCheckpoint(t, root, "process-fixture.json", fixture)
	var killed []liveTrashMutation
	for _, stage := range []string{"abort", "delete", "finish", "verify"} {
		func() {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			log, err := os.OpenFile(filepath.Join(root, "process-"+stage+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal("fresh private child log required")
			}
			defer log.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			childCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			command := exec.CommandContext(childCtx, executable, "-test.run=^TestLiveCOSTrashWorkerProcessChild$", "-test.v", "-test.timeout=85s")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "MYSQL_TEST_") && !strings.HasPrefix(value, "ALBUM_TRASH_PROCESS_") {
					command.Env = append(command.Env, value)
				}
			}
			command.Env = append(command.Env, "ALBUM_TRASH_PROCESS_CHILD="+stage, "ALBUM_TRASH_PROCESS_DSN="+dsn)
			command.Stdout, command.Stderr, command.ExtraFiles = log, log, []*os.File{write, lease}
			if err := command.Start(); err != nil {
				t.Fatal("child start failed")
			}
			defer func() {
				if command.ProcessState == nil {
					command.Process.Kill()
					command.Wait()
				}
			}()
			write.Close()
			if stage == "abort" || stage == "delete" {
				type observed struct {
					mutation liveTrashMutation
					err      error
				}
				ready := make(chan observed, 1)
				go func() { var m liveTrashMutation; err := json.NewDecoder(read).Decode(&m); ready <- observed{m, err} }()
				var m liveTrashMutation
				select {
				case result := <-ready:
					if result.err != nil || result.mutation.Kind != stage {
						t.Fatal("child did not reach a closed provider mutation; retain intent")
					}
					m = result.mutation
				case <-childCtx.Done():
					t.Fatal("child boundary timed out; inspect retained intent")
				}
				used, reserved := photoJournalBalance(t, db, in.Owner)
				var receipts int
				if err := db.QueryRow(`SELECT COUNT(*) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id=?`, in.Owner, in.Asset).Scan(&receipts); err != nil || used != 4096 || reserved != 0 || receipts != 0 {
					t.Fatal("capacity was refunded before the held mutation was committed")
				}
				p, err := NewMySQLUploads(db).objectPurge(ctx, in.Owner, in.Asset)
				if err != nil || p.validate() != nil || p.State != "running" || p.Reason != "trash" || !liveTrashPendingMutation(p, m) {
					t.Fatal("interrupted work lacks a durable purge grant and plan")
				}
				photoMultipartProbeCheckpoint(t, root, "process-"+stage+"-before-kill.json", map[string]any{"mutation": m, "used": used, "reserved": reserved, "receipts": receipts, "purge": p})
				if command.Process.Kill() != nil {
					t.Fatal("child kill failed")
				}
				var exit *exec.ExitError
				if !errors.As(command.Wait(), &exit) {
					t.Fatal("child did not terminate by signal")
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("child exit was not SIGKILL")
				}
				photoMultipartProbeCheckpoint(t, root, "process-"+stage+"-killed.json", map[string]any{"signal": "SIGKILL", "pid": command.Process.Pid, "reaped": true, "mutation": m})
				killed = append(killed, m)
			} else if err := command.Wait(); err != nil {
				t.Fatal("recovery child failed; inspect private log")
			}
		}()
	}
	var completed, verified liveTrashProcessResult
	roundtripRead(t, root, "process-finish-result.json", &completed)
	roundtripRead(t, root, "process-verify-result.json", &verified)
	if !completed.Passed || !verified.Passed || completed.Settled != 1 || verified.Settled != 0 || len(killed) != 2 {
		t.Fatal("cross-process results incomplete")
	}
	retried := false
	for _, v := range completed.Deletes {
		retried = retried || v.Key == killed[1].Key && v.VersionID == killed[1].VersionOrUpload
	}
	var abortedAgain liveTrashMutation
	roundtripRead(t, root, "process-delete-mutation-1.json", &abortedAgain)
	if !retried || abortedAgain != killed[0] {
		t.Fatal("recovery did not retry the same immutable operation")
	}
	for _, key := range in.Keys[:2] {
		versions, err := cleaner.listPurgeVersions(ctx, in.Owner, purgeTarget{key})
		uploads, uploadErr := cleaner.listPurgeUploads(ctx, in.Owner, purgeTarget{key})
		if err != nil || uploadErr != nil || len(versions)+len(uploads) != 0 {
			t.Fatal("resumed worker left target resources unresolved")
		}
	}
	liveTrashReadVersion(t, ctx, reader, in.Versions[3])
	photoMultipartProbeCheckpoint(t, root, "process-result.json", map[string]any{"passed": true, "realCOS": true, "realMySQL": true, "sigkillCount": len(killed), "sameAbortRetried": true, "sameVersionRetried": true, "deletionReceipts": verified.Receipts, "usedBytes": verified.Used, "reservedBytes": verified.Reserved, "postSettlementRestartMutations": len(verified.Deletes) + verified.Aborts, "siblingPreserved": true, "appOrSubscriptionAcceptance": false})
}
