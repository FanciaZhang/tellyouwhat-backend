package albums

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Targets come from the server's closed upload journals, never a client prefix.
type purgeTarget struct {
	Key string `json:"key"`
}

func (t purgeTarget) valid(owner string) bool {
	if !validOwner(owner) {
		return false
	}
	p := strings.Split(t.Key, "/")
	if len(p) == 6 && p[0] == "albums" && p[1] == "staging" && p[2] == "live-pairs" {
		return p[3] == owner && validOwner(p[4]) && (p[5] == "photo" || p[5] == "motion")
	}
	if len(p) != 5 || p[0] != "albums" || p[2] != owner || !validOwner(p[3]) {
		return false
	}
	switch p[1] {
	case "originals", "staging":
		return resourceIDPattern.MatchString(p[4])
	case "derivatives":
		rule, _, _, _ := photoPresetRule(PhotoPreset(p[4]))
		return rule != ""
	case "video-previews":
		return VideoPreset(p[4]) == VideoDaily1080
	case "live-pairs":
		return p[4] == "photo" || p[4] == "motion"
	}
	return false
}

type purgePlan struct {
	Versions []DerivedObjectVersion `json:"versions,omitempty"`
	Uploads  []string               `json:"uploads,omitempty"`
}

func (p purgePlan) validate() error {
	if len(p.Uploads) > 1000 || (len(p.Uploads) != 0 && len(p.Versions) != 0) ||
		validateCleanupPlan(DerivedCleanupPlan{Terminal: true, Versions: p.Versions}) != nil {
		return ErrConflict
	}
	seen := map[string]bool{}
	for _, id := range p.Uploads {
		if !validCOSMultipartID(id) || seen[id] {
			return ErrConflict
		}
		seen[id] = true
	}
	return nil
}

type objectPurge struct {
	OwnerID, AssetID, State, LeaseToken string
	Revision                            int64
	Reason, Environment                 string
	SubscriptionRevision                int64
	VerificationUntil, StartedAt        time.Time
	ExpiresAt, LeaseUntil               time.Time
	Targets                             []purgeTarget
	Index                               int
	ActionIndex                         int
	Plan                                *purgePlan
}

func (p objectPurge) validate() error {
	if p.Reason == "trash" {
		if p.Environment != "" || p.SubscriptionRevision != 0 || !p.VerificationUntil.IsZero() || !p.StartedAt.IsZero() {
			return ErrConflict
		}
	} else if p.Reason != "retention" || (p.Environment != "Sandbox" && p.Environment != "Production") || p.SubscriptionRevision <= 0 || p.VerificationUntil.IsZero() {
		return ErrConflict
	}

	if p.State == "completed" {
		if !validOwner(p.OwnerID) || !resourceIDPattern.MatchString(p.AssetID) || p.Revision <= 0 || p.ExpiresAt.IsZero() || len(p.Targets) != 0 || p.Index != 0 || p.ActionIndex != 0 || p.Plan != nil || p.LeaseToken != "" || !p.LeaseUntil.IsZero() {
			return ErrConflict
		}
		return nil
	}
	if !validOwner(p.OwnerID) || !resourceIDPattern.MatchString(p.AssetID) || p.Revision <= 0 || p.ExpiresAt.IsZero() ||
		len(p.Targets) == 0 || len(p.Targets) > 10000 || p.Index < 0 || p.Index > len(p.Targets) ||
		(p.State != "queued" && p.State != "running" && p.State != "objects_removed") ||
		(p.State == "running") != (validOwner(p.LeaseToken) && !p.LeaseUntil.IsZero()) ||
		(p.State == "objects_removed") != (p.Index == len(p.Targets)) {
		return ErrConflict
	}
	seen := map[string]bool{}
	for _, t := range p.Targets {
		if !t.valid(p.OwnerID) || seen[t.Key] {
			return ErrConflict
		}
		seen[t.Key] = true
	}
	if p.ActionIndex < 0 || p.Plan == nil && p.ActionIndex != 0 || p.Plan != nil &&
		(p.State == "objects_removed" || p.Plan.validate() != nil || p.ActionIndex > len(p.Plan.Versions)+len(p.Plan.Uploads)) {
		return ErrConflict
	}
	return nil
}

func samePurgeLease(a, b objectPurge, now time.Time) bool {
	x, _ := json.Marshal(a.Targets)
	y, _ := json.Marshal(b.Targets)
	return a.OwnerID == b.OwnerID && a.AssetID == b.AssetID && a.Revision == b.Revision && a.ExpiresAt.Equal(b.ExpiresAt) &&
		a.Reason == b.Reason && a.Environment == b.Environment && a.SubscriptionRevision == b.SubscriptionRevision && a.VerificationUntil.Equal(b.VerificationUntil) && a.StartedAt.Equal(b.StartedAt) && a.Index == b.Index && a.ActionIndex == b.ActionIndex && string(x) == string(y) && a.State == "running" && validOwner(b.LeaseToken) && a.LeaseToken == b.LeaseToken && a.LeaseUntil.After(now)
}

type objectPurgeStorage interface {
	listPurgeVersions(context.Context, string, purgeTarget) ([]DerivedObjectVersion, error)
	listPurgeUploads(context.Context, string, purgeTarget) ([]string, error)
	deletePurgeVersion(context.Context, string, purgeTarget, DerivedObjectVersion) error
	abortPurgeUpload(context.Context, string, purgeTarget, string) error
}

// Not scheduled yet: provider quiescence and backup deletion propagation must
// be verified before activation.
func (r *MySQLUploads) runObjectPurgeStep(ctx context.Context, owner, asset string, storage objectPurgeStorage, now func() time.Time) error {
	if storage == nil || now == nil {
		return ErrInvalidManifest
	}
	p, err := r.claimObjectPurge(ctx, owner, asset, uuid.NewString(), now().UTC())
	if err != nil {
		return err
	}
	target := p.Targets[p.Index]
	if p.Plan == nil {
		uploads, err := storage.listPurgeUploads(ctx, owner, target)
		if err != nil {
			return err
		}
		plan := purgePlan{Uploads: uploads}
		if len(uploads) == 0 {
			plan.Versions, err = storage.listPurgeVersions(ctx, owner, target)
			if err != nil {
				return err
			}
		}
		p, err = r.storeObjectPurgePlan(ctx, p, plan, now().UTC())
		if err != nil {
			return err
		}
	}
	for p.ActionIndex < len(p.Plan.Uploads)+len(p.Plan.Versions) {
		p, err = r.authorizeObjectPurgeAction(ctx, p, now)
		if err != nil {
			return err
		}
		if !p.LeaseUntil.After(now().UTC()) {
			return ErrLease
		}
		if len(p.Plan.Uploads) > 0 {
			err = storage.abortPurgeUpload(ctx, owner, target, p.Plan.Uploads[p.ActionIndex])
		} else {
			err = storage.deletePurgeVersion(ctx, owner, target, p.Plan.Versions[p.ActionIndex])
		}
		if err != nil {
			return err
		}
		p, err = r.ackObjectPurgeAction(ctx, p, now().UTC())
		if err != nil {
			return err
		}
	}
	// Re-list on a later step after any mutation. Only a fresh empty inventory
	// advances the key; an acknowledged DELETE alone does not prove it is empty.
	empty := false
	if len(p.Plan.Uploads) == 0 && len(p.Plan.Versions) == 0 {
		// An empty plan recovered after a crash is not a fresh absence proof.
		uploads, err := storage.listPurgeUploads(ctx, owner, target)
		if err != nil {
			return err
		}
		versions, err := storage.listPurgeVersions(ctx, owner, target)
		if err != nil {
			return err
		}
		empty = len(uploads) == 0 && len(versions) == 0
	}
	return r.finishObjectPurgeStep(ctx, p, empty, now().UTC())
}
