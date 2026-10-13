package albums

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

// CI's documented Transcode input is an object path, not a version-aware URL.
// Originals are server-only, per-upload archive keys. Reject historical/latest
// mismatches before POST and before publication, rather than silently processing
// a newer version. Production additionally requires exclusive archive writers.
func (s *COSObjects) checkVideoSource(ctx context.Context, source SealedObject) error {
	if !albumObjectKey(source.Key, "originals") || !strings.HasSuffix(source.Key, "/"+source.ResourceID) ||
		!concreteCOSVersion(source.VersionID) || source.SizeBytes <= 0 || !validSHA256(source.SHA256) {
		return ErrIncompleteBackup
	}
	response, err := s.client.Object.Head(ctx, source.Key, nil)
	if err != nil {
		return ErrVideoProcessing
	}
	if response == nil || response.StatusCode != http.StatusOK || response.Header.Get("x-cos-version-id") != source.VersionID || response.ContentLength != source.SizeBytes {
		return ErrIncompleteBackup
	}
	return nil
}

func (s *COSObjects) InspectVideoSource(ctx context.Context, source SealedObject) (VideoProfile, error) {
	if err := s.checkVideoSource(ctx, source); err != nil {
		return VideoProfile{}, err
	}
	return s.videoInfo(ctx, source.Key, source.VersionID, source.SizeBytes, false)
}

func (s *COSObjects) videoInfo(ctx context.Context, key, version string, size int64, output bool) (VideoProfile, error) {
	result, response, err := s.client.CI.GetMediaInfo(ctx, key, nil, url.QueryEscape(version))
	if err != nil || response == nil || response.StatusCode != http.StatusOK || result == nil || result.MediaInfo == nil {
		return VideoProfile{}, ErrVideoProcessing
	}
	// CI videoinfo can omit the object version header. Both callers HEAD the
	// latest immutable key before this request; recheck after it as well. Keep
	// the explicit versionId query and reject a contradictory header if present.
	if observed := response.Header.Get("x-cos-version-id"); observed != "" && observed != version {
		return VideoProfile{}, ErrVideoProcessing
	}
	profile, err := parseVideoInfo(result.MediaInfo, size, output)
	if err != nil {
		return VideoProfile{}, err
	}
	head, err := s.client.Object.Head(ctx, key, nil)
	if head != nil && head.Body != nil {
		defer head.Body.Close()
	}
	if err != nil || head == nil || head.StatusCode != http.StatusOK || head.Header.Get("x-cos-version-id") != version || head.ContentLength != size {
		return VideoProfile{}, ErrVideoProcessing
	}
	return profile, nil
}

func ciNumber(value string) (float64, error) {
	parts := strings.Split(value, "/")
	if len(parts) > 2 || len(value) > 32 {
		return 0, ErrVideoUnsupported
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, ErrVideoUnsupported
	}
	if len(parts) == 2 {
		d, err := strconv.ParseFloat(parts[1], 64)
		if err != nil || d <= 0 {
			return 0, ErrVideoUnsupported
		}
		v /= d
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, ErrVideoUnsupported
	}
	return v, nil
}

func parseVideoInfo(info *cos.MediaInfo, size int64, output bool) (VideoProfile, error) {
	if info == nil || len(info.Stream.Video) != 1 || len(info.Stream.Audio) > 1 || info.Stream.Subtitle != "" {
		return VideoProfile{}, ErrVideoUnsupported
	}
	actualSize, err := strconv.ParseInt(info.Format.Size, 10, 64)
	if err != nil || actualSize != size || size <= 0 {
		return VideoProfile{}, ErrVideoUnsupported
	}
	v := info.Stream.Video[0]
	if v.CodecName != "h264" && (output || v.CodecName != "hevc") {
		return VideoProfile{}, ErrVideoUnsupported
	}
	// Never implicitly tone-map HDR, discard 10-bit colour, or convert spatial
	// tracks into an ordinary video and call that a space-saving replacement.
	if v.PixFormat != "yuv420p" && v.PixFormat != "yuvj420p" {
		return VideoProfile{}, ErrVideoUnsupported
	}
	for _, value := range []string{v.ColorPrimaries, v.ColorTransfer} {
		if value != "" && value != "unknown" && value != "bt709" && value != "smpte170m" && value != "iec61966-2-1" {
			return VideoProfile{}, ErrVideoUnsupported
		}
	}
	width, widthErr := strconv.Atoi(v.Width)
	height, heightErr := strconv.Atoi(v.Height)
	duration, durationErr := ciNumber(info.Format.Duration)
	fps, fpsErr := ciNumber(v.AvgFps)
	if fpsErr != nil {
		fps, fpsErr = ciNumber(v.Fps)
	}
	if widthErr != nil || heightErr != nil || durationErr != nil || fpsErr != nil || duration > 3600 {
		return VideoProfile{}, ErrVideoUnsupported
	}
	rotation := 0.0
	if v.Rotation != "" {
		rotation, err = strconv.ParseFloat(v.Rotation, 64)
		if err != nil || math.IsNaN(rotation) || math.IsInf(rotation, 0) {
			return VideoProfile{}, ErrVideoUnsupported
		}
		rotation = math.Mod(math.Mod(rotation, 360)+360, 360)
	}
	if rotation != 0 && rotation != 90 && rotation != 180 && rotation != 270 {
		return VideoProfile{}, ErrVideoUnsupported
	}
	if rotation == 90 || rotation == 270 {
		width, height = height, width
	}
	p := VideoProfile{Width: width, Height: height, DurationMilliseconds: int64(math.Round(duration * 1000)), FrameRate: fps}
	// CI represents a silent movie as <Audio/> rather than omitting the node.
	// Only the completely empty SDK record means no audio; partial records fail.
	if len(info.Stream.Audio) == 1 && !reflect.ValueOf(info.Stream.Audio[0]).IsZero() {
		audio := info.Stream.Audio[0]
		p.AudioChannels, err = strconv.Atoi(audio.Channel)
		if err != nil || p.AudioChannels < 1 || audio.CodecName == "" || (output && audio.CodecName != "aac") {
			return VideoProfile{}, ErrVideoUnsupported
		}
	}
	if !p.valid() {
		return VideoProfile{}, ErrVideoUnsupported
	}
	return p, nil
}

func videoUserData(j VideoJob) string { return "albums-video-v1:" + j.ID + ":" + j.ManifestDigest }

func (s *COSObjects) SubmitVideoOnce(ctx context.Context, j VideoJob) (string, error) {
	key, err := j.outputKey()
	if err != nil || j.SubmittedAt.IsZero() || j.Spec == nil || j.validateSpec(*j.Spec) != nil {
		return "", ErrInvalidManifest
	}
	if err := s.checkVideoSource(ctx, j.Spec.Source); err != nil {
		return "", err
	}
	bitrate := videoBitrate(j.Spec.Profile)
	width, height := videoCanvas(j.Spec.Profile)
	// Leave Fps unset to retain the input rate. CI rejects decimal strings
	// (including 30.000000); rounding fractional rates would change the video.
	// validateResult still requires the observed output rate to match the source.
	transcode := &cos.Transcode{Container: &cos.Container{Format: "mp4"},
		Video: &cos.Video{Codec: "H.264", Profile: "high", Width: strconv.Itoa(max(width, height)), Height: strconv.Itoa(min(width, height)), LongShortMode: "true",
			Pixfmt: "yuv420p", Bitrate: strconv.FormatInt(bitrate, 10),
			Maxrate: strconv.FormatInt(bitrate*3/2, 10), Bufsize: strconv.FormatInt(bitrate*2, 10)},
		TransConfig: &cos.TransConfig{IsCheckReso: "true", ResoAdjMethod: "0", IsCheckVideoBitrate: "true", VideoBitrateAdjMethod: "0", IsCheckAudioBitrate: "true", AudioBitrateAdjMethod: "0", IsHdr2Sdr: "false"}}
	if j.Spec.Profile.AudioChannels > 0 {
		transcode.Audio = &cos.Audio{Codec: "aac", Bitrate: "128", Channels: strconv.Itoa(j.Spec.Profile.AudioChannels)}
	}
	request := &cos.CreateMediaJobsOptions{Tag: "Transcode", QueueId: j.Spec.QueueID, CallBack: "no",
		Input: &cos.JobInput{Object: j.Spec.Source.Key}, Operation: &cos.MediaProcessJobOperation{
			Transcode: transcode, Output: &cos.JobOutput{Bucket: strings.Split(s.host, ".")[0], Region: strings.Split(s.host, ".")[2], Object: key}, UserData: videoUserData(j)}}
	result, response, err := s.client.CI.CreateMediaJobs(ctx, request)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || result == nil || !s.matchesVideoJob(j, result.JobsDetail) {
		return "", ErrVideoUncertain
	}
	return result.JobsDetail.JobId, nil
}

func (s *COSObjects) matchesVideoJob(j VideoJob, detail *cos.MediaProcessJobDetail) bool {
	key, err := j.outputKey()
	if err != nil || j.Spec == nil || detail == nil || !ciJobPattern.MatchString(detail.JobId) || detail.Tag != "Transcode" ||
		detail.QueueId != j.Spec.QueueID || detail.Input == nil || detail.Input.Object != j.Spec.Source.Key || detail.Input.Url != "" ||
		detail.Operation == nil || detail.Operation.UserData != videoUserData(j) || detail.Operation.Output == nil {
		return false
	}
	output := detail.Operation.Output
	return output.Object == key && output.Bucket == strings.Split(s.host, ".")[0] && output.Region == strings.Split(s.host, ".")[2]
}

func (s *COSObjects) FindVideoSubmission(ctx context.Context, j VideoJob) (string, error) {
	if j.Spec == nil || j.SubmittedAt.IsZero() || j.validateSpec(*j.Spec) != nil {
		return "", ErrInvalidManifest
	}
	// Complete the bounded scan before attaching a match: a first match alone
	// does not prove another task was not also accepted. Never POST in recovery.
	next, found := "", ""
	seen := make(map[string]bool)
	for range 5 {
		result, response, err := s.client.CI.DescribeMediaJobs(ctx, &cos.DescribeMediaJobsOptions{QueueId: j.Spec.QueueID, Tag: "Transcode", Size: 100, NextToken: next,
			StartCreationTime: j.SubmittedAt.Add(-2 * time.Minute).UTC().Format("2006-01-02T15:04:05-0700"), EndCreationTime: j.SubmittedAt.Add(15 * time.Minute).UTC().Format("2006-01-02T15:04:05-0700")})
		if err != nil || response == nil || response.StatusCode != http.StatusOK || result == nil || len(result.JobsDetail) > 100 {
			return "", ErrVideoProcessing
		}
		for i := range result.JobsDetail {
			detail := &result.JobsDetail[i]
			if s.matchesVideoJob(j, detail) {
				if found != "" && found != detail.JobId {
					return "", ErrVideoFailed
				}
				found = detail.JobId
			}
		}
		if result.NextToken == "" {
			return found, nil
		}
		if seen[result.NextToken] {
			return "", ErrVideoFailed
		}
		seen[result.NextToken] = true
		next = result.NextToken
	}
	return "", ErrVideoFailed // too many tasks to prove uniqueness within budget
}

func (s *COSObjects) PollVideoSubmission(ctx context.Context, j VideoJob) (bool, error) {
	if j.Spec == nil || j.validateSpec(*j.Spec) != nil || !ciJobPattern.MatchString(j.ProviderJobID) {
		return false, ErrInvalidManifest
	}
	result, response, err := s.client.CI.DescribeMediaJob(ctx, j.ProviderJobID)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || result == nil {
		return false, ErrVideoProcessing
	}
	if !s.matchesVideoJob(j, result.JobsDetail) || result.JobsDetail.JobId != j.ProviderJobID {
		return false, ErrVideoFailed
	}
	switch result.JobsDetail.State {
	case "Submitted", "Running", "Pause":
		return false, nil
	case "Success":
		return true, nil
	default:
		return false, ErrVideoFailed
	}
}

func (s *COSObjects) VerifyVideoResult(ctx context.Context, j VideoJob) (VideoResult, error) {
	key, err := j.outputKey()
	if err != nil || j.Spec == nil || j.validateSpec(*j.Spec) != nil {
		return VideoResult{}, ErrInvalidManifest
	}
	if err := s.checkVideoSource(ctx, j.Spec.Source); err != nil {
		return VideoResult{}, err
	}
	head, err := s.client.Object.Head(ctx, key, nil)
	if err != nil || head == nil || head.StatusCode != http.StatusOK || !concreteCOSVersion(head.Header.Get("x-cos-version-id")) || head.ContentLength <= 0 || head.ContentLength > j.Spec.MaxBytes {
		return VideoResult{}, ErrVideoProcessing
	}
	version, size := head.Header.Get("x-cos-version-id"), head.ContentLength
	profile, err := s.videoInfo(ctx, key, version, size, true)
	if err != nil {
		return VideoResult{}, err
	}
	response, err := s.client.Object.Get(ctx, key, nil, url.QueryEscape(version))
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK || response.Header.Get("x-cos-version-id") != version || response.ContentLength != size ||
		response.Header.Get("Content-Range") != "" || (response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return VideoResult{}, ErrVideoProcessing
	}
	// Stream the whole fixed version without buffering the video in RAM. ftyp
	// checks the promised container; native decode/playback remains a client gate.
	var prefix [16]byte
	if _, err := io.ReadFull(response.Body, prefix[:]); err != nil || string(prefix[4:8]) != "ftyp" || binary.BigEndian.Uint32(prefix[:4]) < 16 {
		return VideoResult{}, ErrVideoProcessing
	}
	hash := sha256.New()
	_, _ = hash.Write(prefix[:])
	n, err := io.CopyBuffer(hash, io.LimitReader(response.Body, size-16+1), make([]byte, 64<<10))
	if err != nil || n+16 != size || ctx.Err() != nil {
		return VideoResult{}, ErrVideoProcessing
	}
	result := VideoResult{Object: SealedObject{ResourceID: string(j.Preset), Key: key, VersionID: version, SizeBytes: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, Profile: profile}
	if err := j.validateResult(result); err != nil {
		return VideoResult{}, err
	}
	if err := s.checkVideoSource(ctx, j.Spec.Source); err != nil {
		return VideoResult{}, err
	}
	return result, nil
}

// Assert interfaces at build time; provider errors never expose signed requests.
var _ VideoProcessor = (*COSObjects)(nil)
