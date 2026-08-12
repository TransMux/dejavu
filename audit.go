// DejaVu - Data snapshot and sync.
// Copyright (c) 2022-present, b3log.org
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dejavu

import (
	"sync/atomic"
	"time"
)

const SyncAuditContextKey = "syncAuditRecorder"
const SyncAuditParentContextKey = "syncAuditParent"

type SyncAuditEvent struct {
	Sequence   int64             `json:"sequence"`
	Operation  string            `json:"operation"`
	StartedAt  time.Time         `json:"startedAt"`
	DurationNS int64             `json:"durationNS"`
	Result     string            `json:"result"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type SyncAuditRecorder interface {
	RecordSyncAuditEvent(event SyncAuditEvent)
}

var syncAuditSequence atomic.Int64

func BeginSyncAudit(context map[string]interface{}, operation string, attributes map[string]string) func(error) {
	if context == nil {
		return func(error) {}
	}
	recorder, ok := context[SyncAuditContextKey].(SyncAuditRecorder)
	if !ok || recorder == nil {
		return func(error) {}
	}
	if parent, _ := context[SyncAuditParentContextKey].(string); parent != "" {
		if attributes == nil {
			attributes = map[string]string{}
		}
		attributes["parent"] = parent
	}
	started := time.Now()
	return func(err error) {
		result := "success"
		if err != nil {
			result = "failure"
		}
		recorder.RecordSyncAuditEvent(SyncAuditEvent{Sequence: syncAuditSequence.Add(1), Operation: operation,
			StartedAt: started, DurationNS: time.Since(started).Nanoseconds(), Result: result, Attributes: attributes})
	}
}
