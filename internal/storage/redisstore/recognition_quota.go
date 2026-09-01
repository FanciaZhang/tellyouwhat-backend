package redisstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
)

type RecognitionQuotaStore struct {
	client *redis.Client
	appID  string
}

func NewRecognitionQuotaStore(client *redis.Client, appIDs ...string) *RecognitionQuotaStore {
	appID := "health"
	if len(appIDs) > 0 && appIDs[0] != "" {
		appID = appIDs[0]
	}
	return &RecognitionQuotaStore{client: client, appID: appID}
}

var reserveRecognitionSessionScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local candidateStart = tonumber(ARGV[2])
local candidateEnd = tonumber(ARGV[3])
local limit = tonumber(ARGV[4])
local maximumAge = tonumber(ARGV[5])
local sessionField = 's:' .. ARGV[6]
local endAt = tonumber(redis.call('HGET', KEYS[1], '_end') or '0')
if endAt == 0 or now >= endAt then
  redis.call('DEL', KEYS[1])
  redis.call('HSET', KEYS[1], '_start', candidateStart, '_end', candidateEnd)
  endAt = candidateEnd
end
local completed = 0
local reserved = 0
local existing = nil
local values = redis.call('HGETALL', KEYS[1])
for index = 1, #values, 2 do
  local field = values[index]
  local state = values[index + 1]
  if string.sub(field, 1, 2) == 's:' then
    if state == 'c' then
      completed = completed + 1
      if field == sessionField then existing = state end
    else
      local expiresAt = tonumber(string.sub(state, 3)) or 0
      if now < expiresAt then
        reserved = reserved + 1
        if field == sessionField then existing = state end
      else
        redis.call('HDEL', KEYS[1], field)
      end
    end
  end
end
if existing ~= nil then
  redis.call('EXPIREAT', KEYS[1], endAt + 60)
  return {1, completed, reserved, math.max(0, limit - completed - reserved), endAt}
end
if completed + reserved >= limit then
  return {2, completed, reserved, 0, endAt}
end
local expiresAt = math.min(now + maximumAge, endAt)
redis.call('HSET', KEYS[1], sessionField, 'r:' .. expiresAt)
redis.call('EXPIREAT', KEYS[1], endAt + 60)
return {1, completed, reserved + 1, math.max(0, limit - completed - reserved - 1), endAt}
`)

func (store *RecognitionQuotaStore) Reserve(
	ctx context.Context,
	request recognitionquota.Request,
	now time.Time,
) (recognitionquota.Snapshot, error) {
	if store == nil || store.client == nil || request.DeviceID == "" || uuid.Validate(request.Context.SessionID) != nil {
		return recognitionquota.Snapshot{}, recognitionquota.ErrInvalid
	}
	start, end, err := recognitionquota.BusinessWindow(now, recognitionquota.WindowSettings{
		BusinessDayStartHour: request.Context.BusinessDayStartHour,
		TimeZoneIdentifier:   request.Context.TimeZoneIdentifier,
	})
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	values, err := reserveRecognitionSessionScript.Run(ctx, store.client, []string{store.recognitionWindowKey(request.DeviceID)},
		now.UTC().Unix(), start.Unix(), end.Unix(), recognitionquota.DailySessionLimit,
		int64(recognitionquota.MaximumSessionAge/time.Second), request.Context.SessionID,
	).Slice()
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	code, snapshot, err := recognitionScriptResult(values)
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	if code == 2 {
		return snapshot, recognitionquota.ErrExceeded
	}
	if code != 1 {
		return recognitionquota.Snapshot{}, errors.New("unexpected recognition reservation result")
	}
	return snapshot, nil
}

var completeRecognitionSessionScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local sessionField = 's:' .. ARGV[3]
local endAt = tonumber(redis.call('HGET', KEYS[1], '_end') or '0')
if endAt == 0 or now >= endAt then return {0, 0, 0, limit, endAt} end
local target = redis.call('HGET', KEYS[1], sessionField)
if target == false then return {0, 0, 0, limit, endAt} end
if target ~= 'c' then
  local expiresAt = tonumber(string.sub(target, 3)) or 0
  if now >= expiresAt then
    redis.call('HDEL', KEYS[1], sessionField)
    return {0, 0, 0, limit, endAt}
  end
  redis.call('HSET', KEYS[1], sessionField, 'c')
end
local completed = 0
local reserved = 0
local values = redis.call('HGETALL', KEYS[1])
for index = 1, #values, 2 do
  local field = values[index]
  local state = values[index + 1]
  if string.sub(field, 1, 2) == 's:' then
    if state == 'c' then
      completed = completed + 1
    else
      local expiresAt = tonumber(string.sub(state, 3)) or 0
      if now < expiresAt then reserved = reserved + 1 else redis.call('HDEL', KEYS[1], field) end
    end
  end
end
redis.call('EXPIREAT', KEYS[1], endAt + 60)
return {1, completed, reserved, math.max(0, limit - completed - reserved), endAt}
`)

func (store *RecognitionQuotaStore) Complete(
	ctx context.Context,
	deviceID,
	sessionID string,
	now time.Time,
) (recognitionquota.Snapshot, error) {
	if store == nil || store.client == nil || deviceID == "" || uuid.Validate(sessionID) != nil {
		return recognitionquota.Snapshot{}, recognitionquota.ErrInvalid
	}
	values, err := completeRecognitionSessionScript.Run(ctx, store.client, []string{store.recognitionWindowKey(deviceID)},
		now.UTC().Unix(), recognitionquota.DailySessionLimit, sessionID,
	).Slice()
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	code, snapshot, err := recognitionScriptResult(values)
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	if code == 0 {
		return snapshot, recognitionquota.ErrNotFound
	}
	return snapshot, nil
}

var cancelRecognitionSessionScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local sessionField = 's:' .. ARGV[2]
local endAt = tonumber(redis.call('HGET', KEYS[1], '_end') or '0')
if endAt == 0 or now >= endAt then return 1 end
local target = redis.call('HGET', KEYS[1], sessionField)
if target ~= false and target ~= 'c' then redis.call('HDEL', KEYS[1], sessionField) end
return 1
`)

func (store *RecognitionQuotaStore) Cancel(ctx context.Context, deviceID, sessionID string, now time.Time) error {
	if store == nil || store.client == nil || deviceID == "" || uuid.Validate(sessionID) != nil {
		return recognitionquota.ErrInvalid
	}
	return cancelRecognitionSessionScript.Run(ctx, store.client, []string{store.recognitionWindowKey(deviceID)},
		now.UTC().Unix(), sessionID,
	).Err()
}

var snapshotRecognitionSessionsScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local candidateEnd = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local endAt = tonumber(redis.call('HGET', KEYS[1], '_end') or '0')
if endAt == 0 or now >= endAt then return {1, 0, 0, limit, candidateEnd} end
local completed = 0
local reserved = 0
local values = redis.call('HGETALL', KEYS[1])
for index = 1, #values, 2 do
  local field = values[index]
  local state = values[index + 1]
  if string.sub(field, 1, 2) == 's:' then
    if state == 'c' then
      completed = completed + 1
    else
      local expiresAt = tonumber(string.sub(state, 3)) or 0
      if now < expiresAt then reserved = reserved + 1 else redis.call('HDEL', KEYS[1], field) end
    end
  end
end
return {1, completed, reserved, math.max(0, limit - completed - reserved), endAt}
`)

func (store *RecognitionQuotaStore) Snapshot(
	ctx context.Context,
	deviceID string,
	settings recognitionquota.WindowSettings,
	now time.Time,
) (recognitionquota.Snapshot, error) {
	if store == nil || store.client == nil || deviceID == "" {
		return recognitionquota.Snapshot{}, recognitionquota.ErrInvalid
	}
	_, end, err := recognitionquota.BusinessWindow(now, settings)
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	values, err := snapshotRecognitionSessionsScript.Run(ctx, store.client, []string{store.recognitionWindowKey(deviceID)},
		now.UTC().Unix(), end.Unix(), recognitionquota.DailySessionLimit,
	).Slice()
	if err != nil {
		return recognitionquota.Snapshot{}, err
	}
	_, snapshot, err := recognitionScriptResult(values)
	return snapshot, err
}

func recognitionWindowKey(deviceID string) string {
	return "health:recognition:window:" + deviceID
}

func (store *RecognitionQuotaStore) recognitionWindowKey(deviceID string) string {
	return store.appID + ":recognition:window:" + deviceID
}

func recognitionScriptResult(values []any) (int, recognitionquota.Snapshot, error) {
	if len(values) != 5 {
		return 0, recognitionquota.Snapshot{}, errors.New("invalid recognition quota response")
	}
	parsed := make([]int64, len(values))
	for index, value := range values {
		item, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
		if err != nil {
			return 0, recognitionquota.Snapshot{}, err
		}
		parsed[index] = item
	}
	return int(parsed[0]), recognitionquota.Snapshot{
		Completed: int(parsed[1]), Reserved: int(parsed[2]), Remaining: int(parsed[3]),
		ResetAt: time.Unix(parsed[4], 0).UTC(),
	}, nil
}

var _ recognitionquota.Store = (*RecognitionQuotaStore)(nil)
