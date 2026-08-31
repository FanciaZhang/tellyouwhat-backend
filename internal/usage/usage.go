package usage

import (
	"context"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

type Record struct {
	RequestID     string
	KeyID         string
	DeviceID      string
	TransactionID string
	Operation     contracts.Operation
	InputTokens   int
	OutputTokens  int
	OccurredAt    time.Time
}

type Recorder interface {
	Record(context.Context, Record) error
}

type MemoryRecorder struct {
	mu      sync.Mutex
	records map[string]Record
}

func NewMemoryRecorder() *MemoryRecorder {
	return &MemoryRecorder{records: make(map[string]Record)}
}

func (recorder *MemoryRecorder) Record(_ context.Context, record Record) error {
	recorder.mu.Lock()
	if _, exists := recorder.records[record.RequestID]; !exists {
		recorder.records[record.RequestID] = record
	}
	recorder.mu.Unlock()
	return nil
}
