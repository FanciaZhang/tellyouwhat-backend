package albums

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"time"
)

type VideoPreset string

const VideoDaily1080 VideoPreset = "daily_mp4_1080_v1"

var (
	ErrVideoUnsupported = errors.New("album video format is not supported for processing")
	ErrVideoUncertain   = errors.New("album video submission outcome is unknown")
	ErrVideoProcessing  = errors.New("album video processing operation failed")
	ErrVideoFailed      = errors.New("album video task requires attention")
	ciJobPattern        = regexp.MustCompile(`^j[0-9a-f]{32}$`)
	ciQueuePattern      = regexp.MustCompile(`^p[0-9a-f]{32}$`)
)

// Media information is a bounded provider observation, not evidence of visual
// quality or a certificate authorizing deletion from the system library.
type VideoProfile struct {
	Width                int     `json:"width"`
	Height               int     `json:"height"`
	DurationMilliseconds int64   `json:"durationMilliseconds"`
	FrameRate            float64 `json:"frameRate"`
	AudioChannels        int     `json:"audioChannels"`
}

func (p VideoProfile) valid() bool {
	return p.Width >= 128 && p.Width <= 8192 && p.Height >= 128 && p.Height <= 8192 &&
		p.DurationMilliseconds > 0 && p.DurationMilliseconds <= 3_600_000 &&
		!math.IsNaN(p.FrameRate) && !math.IsInf(p.FrameRate, 0) && p.FrameRate > 0 && p.FrameRate <= 60 &&
		p.AudioChannels >= 0 && p.AudioChannels <= 2
}

func videoCanvas(p VideoProfile) (int, int) {
	if !p.valid() {
		return 0, 0
	}
	scale := min(1.0, 1920.0/float64(max(p.Width, p.Height)), 1080.0/float64(min(p.Width, p.Height)))
	w, h := int(float64(p.Width)*scale)/2*2, int(float64(p.Height)*scale)/2*2
	if w < 128 || h < 128 {
		return 0, 0
	}
	return w, h
}

type VideoTranscodeSpec struct {
	Source          SealedObject     `json:"source"`
	Profile         VideoProfile     `json:"profile"`
	MaxBytes        int64            `json:"maxBytes"`
	QueueID         string           `json:"queueID"`
	LivePhotoTiming *LivePhotoTiming `json:"livePhotoTiming,omitempty"`
}

type VideoResult struct {
	Object  SealedObject `json:"object"`
	Profile VideoProfile `json:"profile"`
}

func (spec VideoTranscodeSpec) matches(other VideoTranscodeSpec) bool {
	return spec.Source == other.Source && spec.Profile == other.Profile && spec.MaxBytes == other.MaxBytes && spec.QueueID == other.QueueID && sameLiveTiming(spec.LivePhotoTiming, other.LivePhotoTiming)
}

func (j VideoJob) outputKey() (string, error) {
	if !validOwner(j.ID) || !validOwner(j.OwnerID) || !validOwner(j.VersionID) || j.Preset != VideoDaily1080 {
		return "", ErrInvalidManifest
	}
	return "albums/video-previews/" + j.OwnerID + "/" + j.ID + "/" + string(j.Preset), nil
}

func validSHA256(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32
}

func (j VideoJob) validateSpec(spec VideoTranscodeSpec) error {
	w, _ := videoCanvas(spec.Profile)
	if _, err := j.outputKey(); err != nil || w == 0 || !ciQueuePattern.MatchString(spec.QueueID) ||
		spec.MaxBytes <= 0 || spec.MaxBytes > 4<<30 || !concreteCOSVersion(spec.Source.VersionID) ||
		spec.Source.SizeBytes <= 0 || !validSHA256(spec.Source.SHA256) || !resourceIDPattern.MatchString(spec.Source.ResourceID) ||
		spec.Source.Key != "albums/originals/"+j.OwnerID+"/"+j.VersionID+"/"+spec.Source.ResourceID {
		return ErrInvalidManifest
	}
	if t := spec.LivePhotoTiming; t != nil && (!t.valid() || t.VideoResourceID != spec.Source.ResourceID || !liveTimingProfileMatches(t, spec.Profile)) {
		return ErrInvalidManifest
	}
	return nil
}

func (j VideoJob) validateResult(result VideoResult) error {
	key, err := j.outputKey()
	if err != nil || j.Spec == nil || j.validateSpec(*j.Spec) != nil || !result.Profile.valid() ||
		result.Object.Key != key || result.Object.ResourceID != string(j.Preset) || !concreteCOSVersion(result.Object.VersionID) ||
		result.Object.SizeBytes <= 0 || result.Object.SizeBytes > j.Spec.MaxBytes || !validSHA256(result.Object.SHA256) {
		return ErrVideoProcessing
	}
	profile, source := result.Profile, j.Spec.Profile
	if j.Spec.LivePhotoTiming != nil && !liveTimingProfileMatches(j.Spec.LivePhotoTiming, profile) {
		return ErrVideoProcessing
	}
	if max(profile.Width, profile.Height) > 1920 || min(profile.Width, profile.Height) > 1080 ||
		profile.Width > source.Width+2 || profile.Height > source.Height+2 ||
		math.Abs(float64(profile.Width)/float64(profile.Height)-float64(source.Width)/float64(source.Height)) > .015*float64(source.Width)/float64(source.Height) ||
		math.Abs(profile.FrameRate-source.FrameRate) > max(.1, .005*source.FrameRate) ||
		math.Abs(float64(profile.DurationMilliseconds-source.DurationMilliseconds)) > max(150, 2000/source.FrameRate) ||
		profile.AudioChannels != source.AudioChannels {
		return ErrVideoProcessing
	}
	return nil
}

type VideoProcessor interface {
	InspectVideoSource(context.Context, SealedObject) (VideoProfile, error)
	SubmitVideoOnce(context.Context, VideoJob) (string, error)
	FindVideoSubmission(context.Context, VideoJob) (string, error)
	PollVideoSubmission(context.Context, VideoJob) (bool, error)
	VerifyVideoResult(context.Context, VideoJob) (VideoResult, error)
}

type VideoJobRepository interface {
	VideoJob(context.Context, string, string, VideoPreset) (VideoJob, error)
	BeginVideoSubmission(context.Context, VideoJob, VideoTranscodeSpec, time.Time) (VideoJob, bool, error)
	AttachVideoSubmission(context.Context, VideoJob, string, time.Time) (VideoJob, error)
	CompleteVideoJob(context.Context, VideoJob, VideoResult, time.Time) (VideoJob, error)
}

func videoSource(v AssetVersion) (SealedObject, error) {
	// Special subtypes (HDR, slow motion, spatial, cinematic) and legacy metadata
	// remain backup-only until their own media and system-import checks exist.
	if v.Manifest.Metadata == nil || (v.Manifest.Kind != Video && !livePhotoPreviewEligible(v.Manifest)) ||
		(v.Manifest.Kind == Video && v.Manifest.Metadata.MediaSubtypes != 0) {
		return SealedObject{}, ErrVideoUnsupported
	}
	role := OriginalVideo
	if v.Manifest.Kind == LivePhoto {
		role = PairedVideo
	}
	if v.Manifest.Edited {
		role = CurrentVideo
	}
	for _, resource := range v.Manifest.Resources {
		if resource.Role != role || resource.Descriptor == nil {
			continue
		}
		for _, object := range v.Objects {
			if object.ResourceID == resource.ID && object.SizeBytes == resource.SizeBytes && object.SHA256 == resource.SHA256 {
				return object, nil
			}
		}
	}
	return SealedObject{}, ErrVideoUnsupported
}

func livePhotoPreviewEligible(m Manifest) bool {
	return m.Kind == LivePhoto && m.LivePhotoTiming != nil && m.Metadata != nil &&
		m.Metadata.MediaSubtypes == 8 && m.Validate() == nil
}

func livePhotoProfileMatches(m Manifest, p VideoProfile) bool {
	if m.Kind != LivePhoto {
		return true
	}
	if !livePhotoPreviewEligible(m) || !p.valid() {
		return false
	}
	return liveTimingProfileMatches(m.LivePhotoTiming, p)
}
func liveTimingProfileMatches(t *LivePhotoTiming, p VideoProfile) bool {
	if t == nil || !t.valid() || !p.valid() {
		return false
	}
	return t.PhotoTimeMicroseconds < p.DurationMilliseconds*1000 &&
		math.Abs(float64(t.DurationMicroseconds)/1000-float64(p.DurationMilliseconds)) <= max(150, 2000/p.FrameRate)
}

// Finish both still components before reserving the movie. Otherwise whichever
// worker wins the race could consume the entire common budget. This is a wait,
// not permission to submit, retry encoding or replace either original resource.
func (s *UploadService) livePhotoMovieBudget(ctx context.Context, owner string, v AssetVersion) (int64, bool, error) {
	r, ok := s.repository.(PhotoWriteRepository)
	if !ok {
		return 0, false, ErrVideoUnsupported
	}
	u, err := s.repository.Get(ctx, owner, v.VersionID)
	if err != nil {
		return 0, false, err
	}
	if u.State != OriginalsVerified || u.Digest != v.ManifestDigest {
		return 0, false, ErrIncompleteBackup
	}
	var used int64
	for _, preset := range photoJobPresets(v.Manifest) {
		p, err := r.PhotoWrite(ctx, owner, v.VersionID, preset)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrPhotoWriteUncertain) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if p.Spec.OwnerID != owner || p.VersionID != v.VersionID || p.ManifestDigest != v.ManifestDigest || p.Spec.Preset != preset {
			return 0, false, ErrConflict
		}
		receipt, err := p.Receipt()
		if errors.Is(err, ErrPhotoWriteUncertain) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if receipt.SizeBytes > u.DerivedBudget-used {
			return 0, false, ErrQuota
		}
		used += receipt.SizeBytes
	}
	if used >= u.DerivedBudget {
		return 0, false, ErrQuota
	}
	return u.DerivedBudget - used, true, nil
}

func videoBitrate(p VideoProfile) int64 {
	if p.FrameRate > 30.1 {
		return 5000
	}
	return 3500
}

// AdvanceVideoJob performs one bounded step, never waits for the full encoding.
// A durable consumed marker grants ONE POST permission. Recovery only discovers
// and polls that attempt; an absent/failed/ambiguous response cannot start anew.
func (s *UploadService) AdvanceVideoJob(ctx context.Context, j VideoJob, queueID string) (bool, error) {
	repository, ok := s.repository.(VideoJobRepository)
	processor, supported := s.objects.(VideoProcessor)
	if !ok || !supported || !ciQueuePattern.MatchString(queueID) {
		return false, ErrVideoUnsupported
	}
	version, err := s.verifiedAssetVersion(ctx, j.OwnerID, j.VersionID)
	if err != nil {
		return false, err
	}
	source, err := videoSource(version)
	if err != nil || j.ManifestDigest != version.ManifestDigest {
		return false, ErrVideoUnsupported
	}
	fresh := false
	if j.SubmittedAt.IsZero() {
		available := s.policy.MaxDerivedBytes
		if version.Manifest.Kind == LivePhoto {
			var ready bool
			available, ready, err = s.livePhotoMovieBudget(ctx, j.OwnerID, version)
			if err != nil || !ready {
				return false, err
			}
		}
		profile, err := processor.InspectVideoSource(ctx, source)
		if err != nil {
			return false, err
		}
		if !profile.valid() || !livePhotoProfileMatches(version.Manifest, profile) {
			return false, ErrVideoUnsupported
		}
		// Maxrate is 1.5 * bitrate; reserve an upper estimate plus container slack.
		// The database rejects insufficient derived quota BEFORE the paid POST.
		cap := (videoBitrate(profile)*1500+128000)*profile.DurationMilliseconds/8000 + (32 << 20)
		// Upload creation reserves at most the originals' size. Do not demand a
		// large fixed allowance for a short clip, or every small video would fail.
		cap = min(cap, source.SizeBytes, s.policy.MaxDerivedBytes, available)
		j, fresh, err = repository.BeginVideoSubmission(ctx, j, VideoTranscodeSpec{Source: source, Profile: profile, MaxBytes: cap, QueueID: queueID, LivePhotoTiming: version.Manifest.LivePhotoTiming}, s.now().UTC())
		if err != nil {
			return false, err
		}
	}
	if j.Spec == nil || j.Spec.Source != source || j.validateSpec(*j.Spec) != nil || !sameLiveTiming(j.Spec.LivePhotoTiming, version.Manifest.LivePhotoTiming) || !livePhotoProfileMatches(version.Manifest, j.Spec.Profile) {
		return false, ErrConflict
	}
	if j.ProviderJobID == "" {
		var id string
		if fresh {
			id, err = processor.SubmitVideoOnce(ctx, j)
		} else {
			id, err = processor.FindVideoSubmission(ctx, j)
		}
		if err != nil {
			return false, err
		}
		if id == "" {
			return false, nil
		}
		j, err = repository.AttachVideoSubmission(ctx, j, id, s.now().UTC())
		if err != nil {
			return false, err
		}
	}
	ready, err := processor.PollVideoSubmission(ctx, j)
	if err != nil || !ready {
		return false, err
	}
	result, err := processor.VerifyVideoResult(ctx, j)
	if err != nil || j.validateResult(result) != nil || !livePhotoProfileMatches(version.Manifest, result.Profile) {
		if err != nil {
			return false, err
		}
		return false, ErrVideoProcessing
	}
	_, err = repository.CompleteVideoJob(ctx, j, result, s.now().UTC())
	return err == nil, err
}
