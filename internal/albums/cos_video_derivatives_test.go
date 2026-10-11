package albums

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func videoJobFixture() VideoJob {
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := VideoJob{ID: uuid.NewString(), OwnerID: uuid.NewString(), VersionID: uuid.NewString(), ManifestDigest: strings.Repeat("a", 64),
		Preset: VideoDaily1080, State: "running", LeaseToken: uuid.NewString(), LeaseUntil: now.Add(10 * time.Minute), Deadline: now.Add(24 * time.Hour), SubmittedAt: now,
		ProviderJobID: "j" + strings.Repeat("b", 32)}
	j.Spec = &VideoTranscodeSpec{Source: SealedObject{ResourceID: "movie", Key: "albums/originals/" + j.OwnerID + "/" + j.VersionID + "/movie", VersionID: "source+version&/=#?", SizeBytes: 12345678, SHA256: strings.Repeat("c", 64)},
		Profile: VideoProfile{1920, 1080, 1000, 60, 2}, MaxBytes: 64 << 20, QueueID: "p" + strings.Repeat("d", 32)}
	return j
}

func mediaInfoFixture(t *testing.T, size int64, profile VideoProfile) *cos.MediaInfo {
	t.Helper()
	audio := ""
	if profile.AudioChannels > 0 {
		audio = fmt.Sprintf(`<Audio><CodecName>aac</CodecName><Channel>%d</Channel></Audio>`, profile.AudioChannels)
	}
	body := fmt.Sprintf(`<Response><MediaInfo><Format><Size>%d</Size><Duration>%.6f</Duration><FormatName>mov,mp4,m4a,3gp,3g2,mj2</FormatName></Format><Stream><Video><CodecName>h264</CodecName><Width>%d</Width><Height>%d</Height><AvgFps>%.6f</AvgFps><Fps>%.6f</Fps><PixFormat>yuv420p</PixFormat><Rotation>0</Rotation><ColorPrimaries>bt709</ColorPrimaries><ColorTransfer>bt709</ColorTransfer></Video>%s</Stream></MediaInfo></Response>`, size, float64(profile.DurationMilliseconds)/1000, profile.Width, profile.Height, profile.FrameRate, profile.FrameRate, audio)
	var result cos.GetMediaInfoResult
	if err := xml.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	return result.MediaInfo
}

func mediaInfoResponse(t *testing.T, r *http.Request, size int64, profile VideoProfile, version string) *http.Response {
	t.Helper()
	body, err := xml.Marshal(cos.GetMediaInfoResult{MediaInfo: mediaInfoFixture(t, size, profile)})
	if err != nil {
		t.Fatal(err)
	}
	return cosResponse(r, string(body), version)
}

func videoDetail(j VideoJob, state string) *cos.MediaProcessJobDetail {
	key, _ := j.outputKey()
	return &cos.MediaProcessJobDetail{JobId: j.ProviderJobID, Tag: "Transcode", QueueId: j.Spec.QueueID, State: state,
		Input: &cos.JobInput{Object: j.Spec.Source.Key}, Operation: &cos.MediaProcessJobOperation{UserData: videoUserData(j), Output: &cos.JobOutput{
			Bucket: "albums-test-1250000000", Region: "ap-guangzhou", Object: key}}}
}

func videoResponse(t *testing.T, r *http.Request, value any) *http.Response {
	t.Helper()
	body, err := xml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return cosResponse(r, string(body), "")
}

func fakeMP4() []byte {
	data := make([]byte, 4096)
	binary.BigEndian.PutUint32(data[:4], 24)
	copy(data[4:8], "ftyp")
	copy(data[8:12], "isom")
	return data
}

func TestCOSVideoInspectionPinsOpaqueSourceVersionAndRejectsHDR(t *testing.T) {
	j := videoJobFixture()
	for _, bad := range []string{"", "source-version", "header-version", "hdr", "multi", "rate"} {
		t.Run(bad, func(t *testing.T) {
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodHead {
					v := j.Spec.Source.VersionID
					if bad == "source-version" {
						v = "new-latest"
					}
					response := cosResponse(r, "", v)
					response.ContentLength = j.Spec.Source.SizeBytes
					return response, nil
				}
				if r.Method != "GET" || r.URL.Query().Get("versionId") != j.Spec.Source.VersionID || r.URL.Query().Get("ci-process") != "videoinfo" {
					t.Fatal("unfixed source information request")
				}
				info := mediaInfoFixture(t, j.Spec.Source.SizeBytes, j.Spec.Profile)
				v := j.Spec.Source.VersionID
				switch bad {
				case "header-version":
					v = "other"
				case "hdr":
					info.Stream.Video[0].ColorTransfer = "smpte2084"
				case "multi":
					info.Stream.Video = append(info.Stream.Video, info.Stream.Video[0])
				case "rate":
					info.Stream.Video[0].AvgFps, info.Stream.Video[0].Fps = "120", "120"
				}
				body, _ := xml.Marshal(cos.GetMediaInfoResult{MediaInfo: info})
				return cosResponse(r, string(body), v), nil
			})
			profile, err := s.InspectVideoSource(context.Background(), j.Spec.Source)
			if bad == "" {
				if err != nil || profile != j.Spec.Profile {
					t.Fatal("valid source rejected", err)
				}
			} else if err == nil {
				t.Fatal("unsafe source admitted")
			}
		})
	}
}

func TestCOSVideoSubmitPreservesFPSAspectAndNeverRetriesLostPOST(t *testing.T) {
	for _, profile := range []VideoProfile{{1920, 1080, 1000, 60, 2}, {1080, 1920, 1000, 30, 1}, {1440, 1080, 1000, 29.97, 0}, {640, 480, 1000, 30, 2}} {
		j := videoJobFixture()
		j.Spec.Profile = profile
		posts := 0
		s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
			if r.Method == "HEAD" {
				response := cosResponse(r, "", j.Spec.Source.VersionID)
				response.ContentLength = j.Spec.Source.SizeBytes
				return response, nil
			}
			posts++
			if r.Method != "POST" || r.URL.Host != "albums-test-1250000000.ci.ap-guangzhou.myqcloud.com" || r.URL.Path != "/jobs" {
				t.Fatal("wrong CI endpoint")
			}
			var request cos.CreateMediaJobsOptions
			if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			v := request.Operation.Transcode.Video
			w, h := videoCanvas(profile)
			if request.QueueId != j.Spec.QueueID || request.CallBack != "no" || request.Input.Object != j.Spec.Source.Key || request.Input.Url != "" ||
				request.Operation.UserData != videoUserData(j) || v.Fps != fmt.Sprintf("%.6f", profile.FrameRate) || v.Width != fmt.Sprint(max(w, h)) || v.Height != fmt.Sprint(min(w, h)) ||
				v.LongShortMode != "true" || v.Codec != "H.264" || request.Operation.Transcode.TransConfig.ResoAdjMethod != "0" || request.Operation.Transcode.TransConfig.IsHdr2Sdr != "false" ||
				(profile.AudioChannels == 0) != (request.Operation.Transcode.Audio == nil) {
				t.Fatal("unbounded or destructive transcode parameters")
			}
			return nil, errors.New("lost response")
		})
		if _, err := s.SubmitVideoOnce(context.Background(), j); !errors.Is(err, ErrVideoUncertain) || posts != 1 {
			t.Fatal("submission retried or fabricated success", posts, err)
		}
	}
}

func TestCOSVideoUnknownSubmissionOnlyQueriesAndProvesUniqueMatch(t *testing.T) {
	for _, scenario := range []string{"found", "absent", "duplicate", "truncated", "cross-source"} {
		t.Run(scenario, func(t *testing.T) {
			j := videoJobFixture()
			reads := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				reads++
				if r.Method != "GET" || r.URL.Path != "/jobs" || r.URL.Query().Get("queueId") != j.Spec.QueueID {
					t.Fatal("recovery performed a mutation")
				}
				detail := *videoDetail(j, "Submitted")
				result := cos.DescribeMediaJobsResult{JobsDetail: []cos.MediaProcessJobDetail{detail}}
				switch scenario {
				case "absent":
					result.JobsDetail = nil
				case "duplicate":
					detail.JobId = "j" + strings.Repeat("e", 32)
					result.JobsDetail = append(result.JobsDetail, detail)
				case "truncated":
					result.NextToken = fmt.Sprint(reads)
				case "cross-source":
					result.JobsDetail[0].Input.Object = "other"
				}
				return videoResponse(t, r, result), nil
			})
			id, err := s.FindVideoSubmission(context.Background(), j)
			if scenario == "found" && (err != nil || id != j.ProviderJobID) {
				t.Fatal("accepted job not recovered", err)
			}
			if (scenario == "absent" || scenario == "cross-source") && (err != nil || id != "") {
				t.Fatal("unbound job recovered", err)
			}
			if (scenario == "duplicate" || scenario == "truncated") && (!errors.Is(err, ErrVideoFailed) || id != "") {
				t.Fatal("ambiguous scan attached a job", err)
			}
			if reads > 5 {
				t.Fatal("unbounded list scan")
			}
		})
	}
}

func TestCOSVideoPollingRejectsForeignAndFailedTasks(t *testing.T) {
	for _, state := range []string{"Submitted", "Running", "Pause", "Success", "Failed", "Cancel", "foreign"} {
		j := videoJobFixture()
		s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.Path != "/jobs/"+j.ProviderJobID {
				t.Fatal("unexpected request")
			}
			detail := videoDetail(j, state)
			if state == "foreign" {
				detail.Operation.UserData = "foreign"
			}
			return videoResponse(t, r, cos.CreateMediaJobsResult{JobsDetail: detail}), nil
		})
		ready, err := s.PollVideoSubmission(context.Background(), j)
		if state == "Success" && (!ready || err != nil) || (state == "Submitted" || state == "Running" || state == "Pause") && (ready || err != nil) ||
			(state == "Failed" || state == "Cancel" || state == "foreign") && (ready || !errors.Is(err, ErrVideoFailed)) {
			t.Fatal("wrong poll classification", state, ready, err)
		}
	}
}

func TestCOSVideoResultStreamsFixedVersionAndChecksMediaAndSource(t *testing.T) {
	for _, scenario := range []string{"valid", "truncated", "long", "wrong-version", "range", "compressed", "container", "fps", "duration", "audio", "source-changed"} {
		t.Run(scenario, func(t *testing.T) {
			j := videoJobFixture()
			data := fakeMP4()
			key, _ := j.outputKey()
			version := "output+version&/=#?"
			heads := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "HEAD" {
					if r.URL.Path == "/"+j.Spec.Source.Key {
						heads++
						v := j.Spec.Source.VersionID
						if scenario == "source-changed" && heads > 1 {
							v = "new-source"
						}
						response := cosResponse(r, "", v)
						response.ContentLength = j.Spec.Source.SizeBytes
						return response, nil
					}
					response := cosResponse(r, "", version)
					response.ContentLength = int64(len(data))
					return response, nil
				}
				if r.URL.Path != "/"+key || r.URL.Query().Get("versionId") != version {
					t.Fatal("output latest used instead of fixed version")
				}
				if r.URL.Query().Get("ci-process") == "videoinfo" {
					profile := j.Spec.Profile
					switch scenario {
					case "fps":
						profile.FrameRate = 30
					case "duration":
						profile.DurationMilliseconds = 500
					case "audio":
						profile.AudioChannels = 0
					}
					return mediaInfoResponse(t, r, int64(len(data)), profile, version), nil
				}
				body := append([]byte(nil), data...)
				switch scenario {
				case "truncated":
					body = body[:len(body)-1]
				case "long":
					body = append(body, 0)
				case "container":
					body[4] = 0
				}
				response := cosResponse(r, string(body), version)
				response.ContentLength = int64(len(data))
				switch scenario {
				case "wrong-version":
					response.Header.Set("x-cos-version-id", "other")
				case "range":
					response.Header.Set("Content-Range", "bytes 0-4095/4096")
				case "compressed":
					response.Header.Set("Content-Encoding", "gzip")
				}
				return response, nil
			})
			result, err := s.VerifyVideoResult(context.Background(), j)
			if scenario != "valid" {
				if err == nil || result.Object.Key != "" {
					t.Fatal("invalid media published")
				}
				return
			}
			digest := sha256.Sum256(data)
			if err != nil || result.Object.VersionID != version || result.Object.SizeBytes != int64(len(data)) || result.Object.SHA256 != hex.EncodeToString(digest[:]) || result.Profile != j.Spec.Profile || heads != 2 {
				t.Fatal("wrong verified result", err)
			}
		})
	}
}

func TestVideoProfileRejectsNonFiniteAndExtremeCanvasAndPreservesRotation(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "0", "1/0", "0/0", "1/2/3"} {
		if _, err := ciNumber(value); err == nil {
			t.Fatal("bad rate accepted")
		}
	}
	for _, p := range []VideoProfile{{8192, 128, 1000, 30, 0}, {128, 8192, 1000, 30, 0}, {1920, 1080, 1000, math.NaN(), 0}} {
		if w, _ := videoCanvas(p); w != 0 {
			t.Fatal("invalid canvas admitted")
		}
	}
	info := mediaInfoFixture(t, 4000, VideoProfile{1920, 1080, 1000, 30, 2})
	info.Stream.Video[0].Rotation = "-90"
	profile, err := parseVideoInfo(info, 4000, false)
	if err != nil || profile.Width != 1080 || profile.Height != 1920 {
		t.Fatal("rotation discarded", err)
	}
}

func TestCOSVideoGrantSignsOnlyCompletedResultExactVersionGET(t *testing.T) {
	j := videoJobFixture()
	key, _ := j.outputKey()
	j.State = "completed"
	j.Result = &VideoResult{SealedObject{string(j.Preset), key, "output+/=&?", 4096, strings.Repeat("e", 64)}, j.Spec.Profile}
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { t.Fatal("signer did network IO"); return nil, nil })
	now := s.now()
	signed, err := s.AuthorizeVideoPreviewRead(context.Background(), j, now.Add(time.Minute))
	u, parseErr := url.Parse(signed)
	if err != nil || parseErr != nil || u.Query().Get("versionId") != j.Result.Object.VersionID || u.Path != "/"+key || u.Host != s.host {
		t.Fatal("wrong immutable video grant", err)
	}
	j.State = "running"
	if _, err := s.AuthorizeVideoPreviewRead(context.Background(), j, now.Add(time.Minute)); !errors.Is(err, ErrVideoNotReady) {
		t.Fatal("unfinished grant accepted")
	}
}
